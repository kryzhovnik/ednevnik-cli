package parse

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/kryzhovnik/ednevnik/internal/model"
	xhtml "golang.org/x/net/html"
)

var (
	ErrInvalidSource = errors.New("portal source is invalid or unsupported")

	studentIDPattern  = regexp.MustCompile(`[?&]student=(\d+)`)
	subjectIDPattern  = regexp.MustCompile(`/grades/(\d+)/show`)
	yearPattern       = regexp.MustCompile(`\b(\d{2})/(\d{2})\b`)
	classPattern      = regexp.MustCompile(`\b([IVX]+\s*[[:alpha:]А-Яа-я])\b`)
	datePattern       = regexp.MustCompile(`\b\d{2}\s*\.\s*\d{2}\s*\.\s*\d{4}\s*\.`)
	gradeValuePattern = regexp.MustCompile(`^[1-5]$`)
)

func invalidSource(message string) error { return fmt.Errorf("%w: %s", ErrInvalidSource, message) }

type Link struct {
	Text        string `json:"text"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url"`
}

type Page struct {
	Title string `json:"title"`
	Text  string `json:"text"`
	Links []Link `json:"links"`
}

func Document(body []byte) (*goquery.Document, error) {
	return goquery.NewDocumentFromReader(bytes.NewReader(body))
}

func GenericPage(body []byte, baseURL string) (Page, error) {
	doc, err := Document(body)
	if err != nil {
		return Page{}, err
	}
	base, _ := url.Parse(baseURL)
	p := Page{Title: clean(doc.Find("title").First().Text()), Text: clean(doc.Find("body").Text())}
	doc.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		u, err := base.Parse(href)
		if err == nil {
			href = u.String()
		}
		description := ""
		for _, attribute := range []string{"aria-label", "title", "data-original-title"} {
			if value, ok := s.Attr(attribute); ok && clean(value) != "" {
				description = clean(value)
				break
			}
		}
		p.Links = append(p.Links, Link{Text: clean(s.Text()), Description: description, URL: href})
	})
	return p, nil
}

func Students(body []byte, currentID string) ([]model.Student, error) {
	doc, err := Document(body)
	if err != nil {
		return nil, err
	}
	if currentID == "" {
		currentID, _ = doc.Find("timeline").First().Attr(":student-class-id")
	}
	byID := map[string]model.Student{}
	invalid := ""
	doc.Find(`a.student-school-class-wrap`).Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		match := studentIDPattern.FindStringSubmatch(href)
		id := ""
		if len(match) == 2 {
			id = match[1]
		} else if s.HasClass("active") {
			id = currentID
		}
		if id == "" {
			invalid = "student entry has no enrolment identifier"
			return
		}
		card := s.Closest(".card.student")
		name := clean(card.Find(".card-header h5").First().Clone().Children().Remove().End().Text())
		items := selectionTexts(s.Find(".student-school-class-item"))
		text := strings.Join(items, " ")
		student := model.Student{ID: id, Name: name, Selected: id == currentID}
		if len(items) > 0 {
			student.School = items[0]
		}
		if y := yearPattern.FindStringSubmatch(text); len(y) == 3 {
			student.SchoolYear = "20" + y[1] + "/20" + y[2]
		}
		if class := clean(s.Find(".school-class-strong").First().Text()); class != "" {
			student.Class = class
		} else if c := classPattern.FindString(text); c != "" {
			student.Class = clean(c)
		}
		if student.Name == "" {
			student.Name = clean(yearPattern.ReplaceAllString(text, ""))
			student.Name = strings.TrimSpace(strings.Trim(student.Name, "()-"))
		}
		if student.Name == "" || student.School == "" || student.SchoolYear == "" || student.Class == "" {
			invalid = "student entry is missing name, school, class, or school year"
			return
		}
		if old, exists := byID[id]; exists && old != student {
			invalid = "enrolment identifier appears with conflicting student data"
			return
		}
		byID[id] = student
	})
	if invalid != "" {
		return nil, invalidSource("invalid student discovery: " + invalid)
	}
	students := make([]model.Student, 0, len(byID))
	for _, s := range byID {
		students = append(students, s)
	}
	latestYear := ""
	for _, s := range students {
		if s.SchoolYear > latestYear {
			latestYear = s.SchoolYear
		}
	}
	for i := range students {
		students[i].Current = students[i].SchoolYear != "" && students[i].SchoolYear == latestYear && !strings.Contains(students[i].Class, "Исписан")
	}
	sort.Slice(students, func(i, j int) bool {
		if students[i].Current != students[j].Current {
			return students[i].Current
		}
		if students[i].Name != students[j].Name {
			return students[i].Name < students[j].Name
		}
		return students[i].SchoolYear > students[j].SchoolYear
	})
	if len(students) == 0 {
		return nil, invalidSource("no students found; page layout may have changed")
	}
	return students, nil
}

func selectionTexts(selection *goquery.Selection) []string {
	var out []string
	selection.Each(func(_ int, s *goquery.Selection) {
		if text := clean(s.Clone().Children().Remove().End().Text()); text != "" {
			out = append(out, text)
		}
	})
	return out
}

func Subjects(body []byte, expectedEnrolment ...string) ([]model.Subject, error) {
	doc, err := Document(body)
	if err != nil {
		return nil, err
	}
	var subjects []model.Subject
	invalid := ""
	recognized := doc.Find(`.flex-table, .grades-wrap, a[href*="/grades/"][href*="/show"]`).Length() > 0
	if len(expectedEnrolment) > 0 && expectedEnrolment[0] != "" {
		if err := validateSuppliedEnrolment(doc, expectedEnrolment[0], "grade overview"); err != nil {
			return nil, err
		}
	}
	doc.Find(`a[href*="/grades/"][href*="/show"]`).Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		m := subjectIDPattern.FindStringSubmatch(href)
		if len(m) != 2 {
			invalid = "subject link has no subject identifier"
			return
		}
		if len(expectedEnrolment) > 0 && expectedEnrolment[0] != "" {
			u, err := url.Parse(href)
			if err != nil || u.Query().Get("student") != expectedEnrolment[0] {
				invalid = "subject link does not identify the requested enrolment"
				return
			}
		}
		text := clean(s.Find("strong.d-block").First().Text())
		teacher := clean(s.Find("em").First().Text())
		numericGrades := s.Find(".grades-cell-wrap .grade.numeric")
		displayedGrades := selectionTexts(numericGrades)
		if len(displayedGrades) != numericGrades.Length() {
			invalid = "numeric grade overview contains an empty value"
			return
		}
		for _, value := range displayedGrades {
			if !gradeValuePattern.MatchString(value) {
				invalid = "numeric grade overview contains an unsupported value"
				return
			}
		}
		if text == "" {
			text = clean(s.Text())
		}
		if text == "" {
			if label, ok := s.Attr("aria-label"); ok {
				text = clean(label)
			}
		}
		if text == "" {
			invalid = "subject entry has no name"
			return
		}
		if s.Find(".grades-cell-wrap .grade:not(.numeric)").Length() > 0 {
			invalid = "grade overview contains an unsupported assessment form"
			return
		}
		subjects = append(subjects, model.Subject{ID: m[1], Name: text, Teacher: teacher, DisplayedGrades: displayedGrades})
	})
	if invalid != "" {
		return nil, invalidSource(invalid)
	}
	if !recognized {
		return nil, invalidSource("grade overview structure was not recognized")
	}
	if len(subjects) == 0 && !hasClosedClassElement(body, "flex-table") && !hasClosedClassElement(body, "grades-wrap") {
		return nil, invalidSource("empty grade overview container is incomplete")
	}
	return dedupeSubjects(subjects), nil
}

func Grades(body []byte, studentID string, subject model.Subject) ([]model.Grade, error) {
	doc, err := Document(body)
	if err != nil {
		return nil, err
	}
	grades := []model.Grade{}
	fallbackOrdinals := map[string]int{}
	if doc.Find(`.categories-wrap, .category-item-wrap.grade`).Length() == 0 {
		return nil, invalidSource("grade detail structure was not recognized")
	}
	if err := validateSuppliedEnrolment(doc, studentID, "grade detail"); err != nil {
		return nil, err
	}
	if doc.Find(".category-item-wrap.grade:not(.numeric)").Length() > 0 {
		return nil, invalidSource("grade detail contains an unsupported assessment form")
	}
	invalid := ""
	doc.Find(".category-item-wrap.grade.numeric").Each(func(_ int, item *goquery.Selection) {
		value := clean(item.Find(".category-symbol").First().Clone().Children().Remove().End().Text())
		if !gradeValuePattern.MatchString(value) {
			invalid = "numeric grade has an unsupported value"
			return
		}
		dateText := item.Find(".name-subtitle").First().Clone().Children().Remove().End().Text()
		date := clean(datePattern.FindString(dateText))
		if date == "" || studentID == "" || subject.ID == "" || subject.Name == "" {
			invalid = "grade record is missing an essential identifier or date"
			return
		}
		kind := strings.TrimSpace(strings.TrimPrefix(clean(item.Find(".name-subtitle-suffix").First().Text()), "•"))
		note := clean(item.Find(".category-item-bottom-note").First().Text())
		sourceID := sourceRecordID(item)
		id := sourceID
		fallbackOrdinal := 0
		if id == "" {
			anchor := strings.Join([]string{subject.ID, date, kind}, "\x00")
			fallbackOrdinals[anchor]++
			fallbackOrdinal = fallbackOrdinals[anchor]
			id = stableID(studentID, anchor, fmt.Sprintf("%d", fallbackOrdinal))
		}
		grades = append(grades, model.Grade{ID: id, SourceID: sourceID, FallbackOrdinal: fallbackOrdinal, StudentID: studentID, SubjectID: subject.ID, Subject: subject.Name, Value: value, Kind: kind, Date: date, Note: note})
	})
	if invalid != "" {
		return nil, invalidSource(invalid)
	}
	if len(grades) == 0 && !hasClosedClassElement(body, "categories-wrap") {
		return nil, invalidSource("empty grade detail container is incomplete")
	}
	if duplicate := duplicateGradeSourceID(grades); duplicate != "" {
		return nil, invalidSource("grade source identifier is duplicated")
	}
	return grades, nil
}

func Absences(body []byte, studentID string) ([]model.Absence, error) {
	doc, err := Document(body)
	if err != nil {
		return nil, err
	}
	if doc.Find(`.categories-wrap`).Length() == 0 {
		return nil, invalidSource("absence structure was not recognized")
	}
	if err := validateSuppliedEnrolment(doc, studentID, "absence page"); err != nil {
		return nil, err
	}
	var out []model.Absence
	fallbackOrdinals := map[string]int{}
	invalid := ""
	doc.Find(".categories-wrap .category-item-wrap").Each(func(_ int, s *goquery.Selection) {
		subject := clean(s.Find(".name").First().Text())
		dateText := clean(s.Closest(".category-wrap").Find(".category-top").First().Text())
		date := clean(datePattern.FindString(dateText))
		periodNumber := clean(s.Find(".category-symbol").First().Clone().Children().Remove().End().Text())
		period := clean(periodNumber + " " + s.Find(".category-symbol-subtitle").First().Text())
		note := clean(s.Find(".category-item-bottom-note").First().Text())
		if subject == "" || date == "" || period == "" || studentID == "" {
			invalid = "absence record is missing an essential field"
			return
		}
		status := "unknown"
		switch {
		case s.HasClass("red"):
			status = "unexcused"
		case s.HasClass("green"):
			status = "excused"
		}
		if status == "unknown" {
			invalid = "absence record has an unsupported status"
			return
		}
		sourceID := sourceRecordID(s)
		id := sourceID
		fallbackOrdinal := 0
		if id == "" {
			anchor := strings.Join([]string{subject, date, period}, "\x00")
			fallbackOrdinals[anchor]++
			fallbackOrdinal = fallbackOrdinals[anchor]
			id = stableID(studentID, anchor, fmt.Sprintf("%d", fallbackOrdinal))
		}
		out = append(out, model.Absence{ID: id, SourceID: sourceID, FallbackOrdinal: fallbackOrdinal, StudentID: studentID, Subject: subject, Date: date, Period: period, Status: status, Note: note})
	})
	if invalid != "" {
		return nil, invalidSource(invalid)
	}
	if len(out) == 0 && !hasClosedClassElement(body, "categories-wrap") {
		return nil, invalidSource("empty absence container is incomplete")
	}
	if duplicate := duplicateAbsenceSourceID(out); duplicate != "" {
		return nil, invalidSource("absence source identifier is duplicated")
	}
	return out, nil
}

func duplicateGradeSourceID(items []model.Grade) string {
	seen := map[string]bool{}
	for _, item := range items {
		if item.SourceID == "" {
			continue
		}
		if seen[item.SourceID] {
			return item.SourceID
		}
		seen[item.SourceID] = true
	}
	return ""
}

func duplicateAbsenceSourceID(items []model.Absence) string {
	seen := map[string]bool{}
	for _, item := range items {
		if item.SourceID == "" {
			continue
		}
		if seen[item.SourceID] {
			return item.SourceID
		}
		seen[item.SourceID] = true
	}
	return ""
}

func sourceRecordID(item *goquery.Selection) string {
	for _, name := range []string{"data-record-id", "data-id"} {
		if value, ok := item.Attr(name); ok && clean(value) != "" {
			return clean(value)
		}
	}
	return ""
}

func validateSuppliedEnrolment(doc *goquery.Document, expected, section string) error {
	for _, attribute := range []string{"data-student-class-id", ":student-class-id"} {
		var mismatch bool
		doc.Find("*").EachWithBreak(func(_ int, s *goquery.Selection) bool {
			value, _ := s.Attr(attribute)
			if value != "" && value != expected {
				mismatch = true
				return false
			}
			return true
		})
		if mismatch {
			return invalidSource(section + " identifies a different enrolment")
		}
	}
	return nil
}

func hasClosedClassElement(body []byte, className string) bool {
	tokenizer := xhtml.NewTokenizer(bytes.NewReader(body))
	targetTag := ""
	nestedTargets := 0
	for {
		switch tokenizer.Next() {
		case xhtml.ErrorToken:
			return false
		case xhtml.StartTagToken:
			token := tokenizer.Token()
			if targetTag == "" {
				for _, attr := range token.Attr {
					if attr.Key == "class" && slices.Contains(strings.Fields(attr.Val), className) {
						targetTag = token.Data
						nestedTargets = 1
						break
					}
				}
			} else if token.Data == targetTag {
				nestedTargets++
			}
		case xhtml.EndTagToken:
			token := tokenizer.Token()
			if targetTag != "" && token.Data == targetTag {
				nestedTargets--
				if nestedTargets == 0 {
					return true
				}
			}
		}
	}
}

type timelineResponse struct {
	Success bool `json:"success"`
	Meta    struct {
		CurrentPage int  `json:"currentPage"`
		NextPage    *int `json:"nextPage"`
		LastPage    int  `json:"lastPage"`
	} `json:"meta"`
	Data json.RawMessage `json:"data"`
}

type timelineGroup struct {
	Date struct {
		Day string `json:"day"`
	} `json:"date"`
	Items json.RawMessage `json:"items"`
}

type timelineItem struct {
	ID          int64  `json:"id"`
	Date        string `json:"date"`
	TypeName    string `json:"typeName"`
	TypeClass   string `json:"typeClass"`
	Title       string `json:"title"`
	SymbolValue any    `json:"symbolValue"`
	Subtitle    string `json:"subtitle"`
	Note        string `json:"note"`
	IsNew       bool   `json:"isNew"`
	ItemURL     string `json:"itemUrl"`
	ItemType    string `json:"itemType"`
}

// Timeline parses the JSON returned by /timeline-data. Text fields can contain
// small HTML fragments, so they are normalized to plain text for stable output.
func Timeline(body []byte, studentID string, expectedPage ...int) (model.ActivityPage, error) {
	var raw timelineResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return model.ActivityPage{}, invalidSource("timeline response is not valid JSON")
	}
	if !raw.Success {
		return model.ActivityPage{}, invalidSource("timeline request was not successful")
	}
	if raw.Meta.CurrentPage < 1 || raw.Meta.LastPage < raw.Meta.CurrentPage {
		return model.ActivityPage{}, invalidSource("timeline pagination metadata is missing or invalid")
	}
	if len(expectedPage) > 0 && raw.Meta.CurrentPage != expectedPage[0] {
		return model.ActivityPage{}, invalidSource(fmt.Sprintf("timeline returned page %d for requested page %d", raw.Meta.CurrentPage, expectedPage[0]))
	}
	if raw.Meta.CurrentPage < raw.Meta.LastPage {
		if raw.Meta.NextPage == nil || *raw.Meta.NextPage != raw.Meta.CurrentPage+1 {
			return model.ActivityPage{}, invalidSource("timeline next-page metadata is missing or non-sequential")
		}
	} else if raw.Meta.NextPage != nil {
		return model.ActivityPage{}, invalidSource("final timeline page unexpectedly has a next page")
	}
	if len(raw.Data) == 0 || bytes.Equal(bytes.TrimSpace(raw.Data), []byte("null")) {
		return model.ActivityPage{}, invalidSource("timeline data array is missing")
	}
	var groups []timelineGroup
	if err := json.Unmarshal(raw.Data, &groups); err != nil || groups == nil {
		return model.ActivityPage{}, invalidSource("timeline data is not an array")
	}
	page := model.ActivityPage{SchemaVersion: model.SchemaVersion, CurrentPage: raw.Meta.CurrentPage, NextPage: raw.Meta.NextPage, LastPage: raw.Meta.LastPage, Items: []model.Activity{}}
	for _, group := range groups {
		if len(group.Items) == 0 || bytes.Equal(bytes.TrimSpace(group.Items), []byte("null")) {
			return model.ActivityPage{}, invalidSource("timeline group has no items array")
		}
		var items []timelineItem
		if err := json.Unmarshal(group.Items, &items); err != nil || items == nil {
			return model.ActivityPage{}, invalidSource("timeline group items is not an array")
		}
		for _, item := range items {
			typeID := plainText(item.ItemType)
			if typeID == "" {
				typeID = plainText(item.TypeClass)
			}
			symbol := ""
			switch value := item.SymbolValue.(type) {
			case string:
				symbol = plainText(value)
			case float64:
				symbol = fmt.Sprintf("%g", value)
			case nil:
			default:
				return model.ActivityPage{}, invalidSource("timeline item has an unsupported symbol value")
			}
			portalID := fmt.Sprintf("%d", item.ID)
			if item.ID <= 0 || typeID == "" || plainText(item.Title) == "" {
				return model.ActivityPage{}, invalidSource("timeline item is missing an essential identifier or type")
			}
			page.Items = append(page.Items, model.Activity{
				ID: stableID(studentID, typeID, portalID), StudentID: studentID, PortalID: item.ID,
				Date: plainText(item.Date), Day: plainText(group.Date.Day), Type: typeID,
				TypeName: plainText(item.TypeName), Title: plainText(item.Title), Symbol: symbol,
				Subtitle: plainText(item.Subtitle), Note: plainText(item.Note), URL: item.ItemURL, IsNew: item.IsNew,
			})
		}
	}
	return page, nil
}

func plainText(value string) string {
	value = html.UnescapeString(value)
	doc, err := goquery.NewDocumentFromReader(strings.NewReader("<div>" + value + "</div>"))
	if err != nil {
		return clean(value)
	}
	return clean(doc.Find("div").First().Text())
}

func clean(s string) string { return strings.Join(strings.Fields(s), " ") }

func stableID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:12])
}

func dedupeSubjects(in []model.Subject) []model.Subject {
	seen := map[string]bool{}
	out := make([]model.Subject, 0, len(in))
	for _, item := range in {
		if !seen[item.ID] {
			seen[item.ID] = true
			out = append(out, item)
		}
	}
	return out
}
