package parse

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/kryzhovnik/ednevnik/internal/model"
)

var (
	studentIDPattern = regexp.MustCompile(`[?&]student=(\d+)`)
	subjectIDPattern = regexp.MustCompile(`/grades/(\d+)/show`)
	yearPattern      = regexp.MustCompile(`\b(\d{2})/(\d{2})\b`)
	classPattern     = regexp.MustCompile(`\b([IVX]+\s*[[:alpha:]А-Яа-я])\b`)
	datePattern      = regexp.MustCompile(`\b\d{2}\s*\.\s*\d{2}\s*\.\s*\d{4}\s*\.`)
)

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
	byID := map[string]model.Student{}
	doc.Find(`a[href*="student="]`).Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		match := studentIDPattern.FindStringSubmatch(href)
		if len(match) != 2 {
			return
		}
		id := match[1]
		card := s.Closest(".card.student")
		name := clean(card.Find(".card-header h5").First().Clone().Children().Remove().End().Text())
		items := selectionTexts(s.Find(".student-school-class-item"))
		text := strings.Join(items, " ")
		if old, exists := byID[id]; exists && old.Name != "" {
			return
		}
		student := model.Student{ID: id, Name: name, Selected: s.HasClass("active") || id == currentID}
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
		byID[id] = student
	})
	students := make([]model.Student, 0, len(byID))
	for _, s := range byID {
		students = append(students, s)
	}
	latestYear := map[string]string{}
	for _, s := range students {
		if s.SchoolYear > latestYear[s.Name] {
			latestYear[s.Name] = s.SchoolYear
		}
	}
	for i := range students {
		students[i].Current = students[i].SchoolYear != "" && students[i].SchoolYear == latestYear[students[i].Name]
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
		return nil, fmt.Errorf("no students found; page layout may have changed")
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

func Subjects(body []byte) ([]model.Subject, error) {
	doc, err := Document(body)
	if err != nil {
		return nil, err
	}
	var subjects []model.Subject
	doc.Find(`a[href*="/grades/"][href*="/show"]`).Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		m := subjectIDPattern.FindStringSubmatch(href)
		if len(m) != 2 {
			return
		}
		text := clean(s.Find("strong.d-block").First().Text())
		teacher := clean(s.Find("em").First().Text())
		displayedGrades := selectionTexts(s.Find(".grades-cell-wrap .grade.numeric"))
		if text == "" {
			text = clean(s.Text())
		}
		if text == "" {
			if label, ok := s.Attr("aria-label"); ok {
				text = clean(label)
			}
		}
		subjects = append(subjects, model.Subject{ID: m[1], Name: text, Teacher: teacher, DisplayedGrades: displayedGrades})
	})
	if len(subjects) == 0 {
		return nil, fmt.Errorf("no subjects found; page layout may have changed")
	}
	return dedupeSubjects(subjects), nil
}

func Grades(body []byte, studentID string, subject model.Subject) ([]model.Grade, error) {
	doc, err := Document(body)
	if err != nil {
		return nil, err
	}
	grades := []model.Grade{}
	doc.Find(".category-item-wrap.grade.numeric").Each(func(_ int, item *goquery.Selection) {
		value := clean(item.Find(".category-symbol").First().Clone().Children().Remove().End().Text())
		if !regexp.MustCompile(`^[1-5]$`).MatchString(value) {
			return
		}
		dateText := item.Find(".name-subtitle").First().Clone().Children().Remove().End().Text()
		date := clean(datePattern.FindString(dateText))
		kind := strings.TrimSpace(strings.TrimPrefix(clean(item.Find(".name-subtitle-suffix").First().Text()), "•"))
		note := clean(item.Find(".category-item-bottom-note").First().Text())
		id := stableID(studentID, subject.ID, value, date, kind, note)
		grades = append(grades, model.Grade{ID: id, StudentID: studentID, SubjectID: subject.ID, Subject: subject.Name, Value: value, Kind: kind, Date: date, Note: note})
	})
	return grades, nil
}

func Absences(body []byte, studentID string) ([]model.Absence, error) {
	doc, err := Document(body)
	if err != nil {
		return nil, err
	}
	var out []model.Absence
	doc.Find(".categories-wrap .category-item-wrap").Each(func(_ int, s *goquery.Selection) {
		subject := clean(s.Find(".name").First().Text())
		date := clean(s.Find(".name-subtitle").First().Text())
		period := clean(s.Find(".category-symbol-subtitle").First().Text())
		note := clean(s.Find(".category-item-bottom-note").First().Text())
		if subject == "" && date == "" {
			return
		}
		status := "unknown"
		switch {
		case s.HasClass("red"):
			status = "unexcused"
		case s.HasClass("green"):
			status = "excused"
		}
		id := stableID(studentID, subject, date, period, status, note)
		out = append(out, model.Absence{ID: id, StudentID: studentID, Subject: subject, Date: date, Period: period, Status: status, Note: note})
	})
	return dedupeAbsences(out), nil
}

type timelineResponse struct {
	Success bool `json:"success"`
	Meta    struct {
		CurrentPage int  `json:"currentPage"`
		NextPage    *int `json:"nextPage"`
		LastPage    int  `json:"lastPage"`
	} `json:"meta"`
	Data []struct {
		Date struct {
			Day string `json:"day"`
		} `json:"date"`
		Items []struct {
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
		} `json:"items"`
	} `json:"data"`
}

// Timeline parses the JSON returned by /timeline-data. Text fields can contain
// small HTML fragments, so they are normalized to plain text for stable output.
func Timeline(body []byte, studentID string) (model.ActivityPage, error) {
	var raw timelineResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return model.ActivityPage{}, fmt.Errorf("decode timeline JSON: %w", err)
	}
	if !raw.Success {
		return model.ActivityPage{}, fmt.Errorf("timeline request was not successful")
	}
	page := model.ActivityPage{SchemaVersion: model.SchemaVersion, CurrentPage: raw.Meta.CurrentPage, NextPage: raw.Meta.NextPage, LastPage: raw.Meta.LastPage, Items: []model.Activity{}}
	for _, group := range raw.Data {
		for _, item := range group.Items {
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
			}
			portalID := fmt.Sprintf("%d", item.ID)
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

func dedupeAbsences(in []model.Absence) []model.Absence {
	seen := map[string]bool{}
	out := make([]model.Absence, 0, len(in))
	for _, item := range in {
		if !seen[item.ID] {
			seen[item.ID] = true
			out = append(out, item)
		}
	}
	return out
}
