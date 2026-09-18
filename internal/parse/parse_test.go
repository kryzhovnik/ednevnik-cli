package parse

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kryzhovnik/ednevnik/internal/model"
)

func TestStudents(t *testing.T) {
	html := []byte(`<html><body><div class="students-list"><div class="card student"><div class="card-header"><h5>Child One</h5></div><div class="collapse show"><a class="student-school-class-wrap active" href="/?student=1234567"><div class="student-school-class-item">School</div><div class="student-school-class-item school-class-strong">VII b</div><div class="student-school-class-item">26/27</div></a><a class="student-school-class-wrap" href="/?student=1234566"><div class="student-school-class-item">School</div><div class="student-school-class-item school-class-strong">VI b</div><div class="student-school-class-item">25/26</div></a></div></div><div class="card student"><div class="card-header"><h5>Child Two</h5></div><a class="student-school-class-wrap" href="/?student=2345678"><div class="student-school-class-item">Other school</div><div class="student-school-class-item school-class-strong">V a</div><div class="student-school-class-item">26/27</div></a></div></div></body></html>`)
	students, err := Students(html, "1234567")
	if err != nil {
		t.Fatal(err)
	}
	if len(students) != 3 {
		t.Fatalf("got %d students, want 3", len(students))
	}
	if students[0].ID != "1234567" || !students[0].Current || !students[0].Selected {
		t.Fatalf("current student not first: %#v", students[0])
	}
	if students[0].SchoolYear != "2026/2027" {
		t.Fatalf("school year = %q", students[0].SchoolYear)
	}
	if students[0].Name != "Child One" || students[0].School != "School" || students[0].Class != "VII b" {
		t.Fatalf("student = %#v", students[0])
	}
}

func TestStudentsDiscoversFiveChildrenAndKeepsEnrolmentsDistinct(t *testing.T) {
	var cards strings.Builder
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&cards, `<div class="card student"><div class="card-header"><h5>Child %d</h5></div><a class="student-school-class-wrap" href="/?student=%d"><div class="student-school-class-item">School %d</div><div class="student-school-class-item school-class-strong">V %c</div><div class="student-school-class-item">26/27</div></a></div>`, i, 1000000+i, i, 'a'+i-1)
	}
	// A second school year is a separate enrolment, while an exact repeated
	// navigation link must not create another record.
	cards.WriteString(`<div class="card student"><div class="card-header"><h5>Child 1</h5></div><a class="student-school-class-wrap" href="/?student=9000001"><div class="student-school-class-item">School 1</div><div class="student-school-class-item school-class-strong">IV a</div><div class="student-school-class-item">25/26</div></a><a class="student-school-class-wrap" href="/?student=9000001"><div class="student-school-class-item">School 1</div><div class="student-school-class-item school-class-strong">IV a</div><div class="student-school-class-item">25/26</div></a></div>`)
	students, err := Students([]byte(cards.String()), "1000001")
	if err != nil {
		t.Fatal(err)
	}
	if len(students) != 6 {
		t.Fatalf("students=%#v", students)
	}
	if students[0].ID != "1000001" || !students[0].Selected || !students[0].Current {
		t.Fatalf("selected current enrolment=%#v", students[0])
	}
}

func TestStudentsRejectsPartiallyParsedFamily(t *testing.T) {
	body := []byte(`<div class="card student"><div class="card-header"><h5>Child</h5></div><a class="student-school-class-wrap" href="/?student=1234567"><div class="student-school-class-item">School</div><div class="student-school-class-item">26/27</div></a></div>`)
	if _, err := Students(body, ""); err == nil {
		t.Fatal("partial student entry accepted")
	}
}

func TestStudentsUsesCurrentIDForActiveLinkWithoutStudentQuery(t *testing.T) {
	html := []byte(`<timeline :student-class-id="222"></timeline><div class="card student"><div class="card-header"><h5>Mark</h5></div><a class="student-school-class-wrap active" href="/"><div class="student-school-class-item">School</div><div class="student-school-class-item school-class-strong">VII b</div><div class="student-school-class-item">26/27</div></a><a class="student-school-class-wrap" href="/?student=111"><div class="student-school-class-item">School</div><div class="student-school-class-item school-class-strong">VI b</div><div class="student-school-class-item">25/26</div></a></div>`)
	students, err := Students(html, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(students) != 2 || students[0].ID != "222" || !students[0].Current || !students[0].Selected {
		t.Fatalf("students = %#v", students)
	}
}

func TestSubjectsAndGrades(t *testing.T) {
	index := []byte(`<a class="flex-table-row" href="/grades/7654321/show?student=1234567"><div><strong class="d-block">Mathematics</strong><em>Teacher Name</em></div><div class="grades-cell-wrap"><div class="grade numeric">5</div></div></a>`)
	subjects, err := Subjects(index)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 || subjects[0].ID != "7654321" {
		t.Fatalf("subjects = %#v", subjects)
	}
	if subjects[0].Name != "Mathematics" || subjects[0].Teacher != "Teacher Name" || len(subjects[0].DisplayedGrades) != 1 || subjects[0].DisplayedGrades[0] != "5" {
		t.Fatalf("subject = %#v", subjects[0])
	}
	detail := []byte(`<main><h1>Mathematics</h1><section>First semester</section><article>5 Excellent 08. 09. 2026. • Practical NOTE Complete equipment</article></main>`)
	grades, err := Grades(detail, "1234567", model.Subject{ID: "7654321", Name: "Mathematics"})
	if err == nil || grades != nil {
		t.Fatalf("unrecognized detail err=%v grades=%#v", err, grades)
	}
	detail = []byte(`<main><div class="category-item-wrap grade numeric very-good"><div class="category-item-top"><div class="category-symbol">5</div><div class="category-item-info"><div class="name">Одличан</div><div class="name-subtitle">08. 09. 2026.<div class="name-subtitle-suffix">• Практично</div></div></div></div><div class="category-item-bottom"><div class="category-item-bottom-note-title">БЕЛЕШКА</div><div class="category-item-bottom-note">Комплетиран прибор</div></div></div></main>`)
	grades, err = Grades(detail, "1234567", model.Subject{ID: "7654321", Name: "Математика"})
	if err != nil {
		t.Fatal(err)
	}
	if len(grades) != 1 {
		t.Fatalf("grades = %#v", grades)
	}
	if grades[0].Value != "5" || grades[0].Kind != "Практично" || grades[0].Note != "Комплетиран прибор" {
		t.Fatalf("grade = %#v", grades[0])
	}
}

func TestSectionsDistinguishValidEmptyFromUnrecognizedAndUnsupported(t *testing.T) {
	if subjects, err := Subjects([]byte(`<div class="flex-table"></div>`), "1234567"); err != nil || len(subjects) != 0 {
		t.Fatalf("empty grades subjects=%#v err=%v", subjects, err)
	}
	if absences, err := Absences([]byte(`<div class="categories-wrap"></div>`), "1234567"); err != nil || len(absences) != 0 {
		t.Fatalf("empty absences=%#v err=%v", absences, err)
	}
	if _, err := Subjects([]byte(`<html><title>Maintenance</title></html>`), "1234567"); err == nil {
		t.Fatal("maintenance HTML accepted as empty grades")
	}
	if _, err := Absences([]byte(`<html><title>Maintenance</title></html>`), "1234567"); err == nil {
		t.Fatal("maintenance HTML accepted as empty absences")
	}
	unknown := []byte(`<div class="categories-wrap"><div class="category-item-wrap grade descriptive"><div class="category-symbol">A</div></div></div>`)
	if _, err := Grades(unknown, "1234567", model.Subject{ID: "1", Name: "Art"}); err == nil {
		t.Fatal("unsupported assessment form was silently omitted")
	}
}

func TestSubjectsRequireRequestedEnrolmentEvidence(t *testing.T) {
	body := []byte(`<a href="/grades/7654321/show?student=9999999"><strong class="d-block">Math</strong></a>`)
	if _, err := Subjects(body, "1234567"); err == nil {
		t.Fatal("overview for another enrolment accepted")
	}
}

func TestEmptySectionsRejectWrongEnrolmentAndTruncatedContainer(t *testing.T) {
	wrong := []byte(`<div class="flex-table" data-student-class-id="9999999"></div>`)
	if _, err := Subjects(wrong, "1234567"); err == nil {
		t.Fatal("empty overview for another enrolment accepted")
	}
	wrong = []byte(`<div class="categories-wrap" data-student-class-id="9999999"></div>`)
	if _, err := Absences(wrong, "1234567"); err == nil {
		t.Fatal("empty absences for another enrolment accepted")
	}
	if _, err := Subjects([]byte(`<div class="flex-table">`), "1234567"); err == nil {
		t.Fatal("truncated empty overview accepted")
	}
	if _, err := Absences([]byte(`<div class="categories-wrap">`), "1234567"); err == nil {
		t.Fatal("truncated empty absences accepted")
	}
}

func TestGenericPage(t *testing.T) {
	p, err := GenericPage([]byte(`<html><head><title> Page </title></head><body><a href="/grades" aria-label="Grade activity detail"> Grades </a></body></html>`), "https://example.test")
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Page" || len(p.Links) != 1 || p.Links[0].URL != "https://example.test/grades" {
		t.Fatalf("page = %#v", p)
	}
	if p.Links[0].Description != "Grade activity detail" {
		t.Fatalf("description = %q", p.Links[0].Description)
	}
}

func TestAbsences(t *testing.T) {
	body := []byte(`<div class="categories-wrap"><div class="category-wrap"><div class="category-top">08. 09. 2026.</div><div class="category-item-wrap red"><span class="category-symbol-subtitle">2. час</span><div class="name">Mathematics</div><div class="category-item-bottom-note">Late</div></div></div><div class="category-wrap"><div class="category-top">09. 09. 2026.</div><div class="category-item-wrap green"><span class="category-symbol-subtitle">3. час</span><div class="name">Physics</div></div></div></div>`)
	items, err := Absences(body, "1234567")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %#v", items)
	}
	if items[0].Status != "unexcused" || items[0].Subject != "Mathematics" || items[0].Period != "2. час" || items[0].Note != "Late" {
		t.Fatalf("first = %#v", items[0])
	}
	if items[1].Status != "excused" {
		t.Fatalf("second = %#v", items[1])
	}
}

func TestAbsencesReadsDateFromGroupAndFullPeriodFromItem(t *testing.T) {
	body := []byte(`<div class="categories-wrap"><div class="category-wrap"><div class="category-top"><span>уторак</span><strong>08. 09. 2026.</strong></div><div class="category-items-wrap"><div class="category-item-wrap red"><div class="category-symbol">2.<span class="category-symbol-subtitle">Час</span></div><div class="name">Француски језик</div><div class="name-subtitle">Неоправдан изостанак</div><div class="category-item-bottom-note">Bonjour la rentrée</div></div></div></div></div>`)
	items, err := Absences(body, "1234567")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Date != "08. 09. 2026." || items[0].Period != "2. Час" {
		t.Fatalf("items = %#v", items)
	}
}

func TestTimeline(t *testing.T) {
	body := []byte(`{"success":true,"meta":{"currentPage":1,"nextPage":2,"lastPage":3},"data":[{"date":{"date":"11. септембар","timestamp":1,"day":"Петак"},"items":[{"id":42,"date":"11. 09. 2026.","typeName":"Активност","typeClass":"activity blue","title":"Математика","symbolValue":null,"subtitle":"&lt;b&gt;Усмено&lt;/b&gt;","note":"Није усвојио &lt;strong&gt;квадрирање&lt;/strong&gt;.","isNew":true,"itemUrl":"https:\/\/example.test\/activities\/42\/show","itemType":"activity"}]}]}`)
	page, err := Timeline(body, "1234567")
	if err != nil {
		t.Fatal(err)
	}
	if page.CurrentPage != 1 || page.NextPage == nil || *page.NextPage != 2 || len(page.Items) != 1 {
		t.Fatalf("page = %#v", page)
	}
	item := page.Items[0]
	if item.Type != "activity" || item.Title != "Математика" || item.Subtitle != "Усмено" || item.Note != "Није усвојио квадрирање." || !item.IsNew {
		t.Fatalf("item = %#v", item)
	}
}

func TestTimelineRejectsSuccessWithoutCoverageMetadata(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"success":true,"data":[]}`),
		[]byte(`{"success":true,"meta":{"currentPage":1,"lastPage":1}}`),
		[]byte(`{"success":true,"meta":{"currentPage":1,"lastPage":1},"data":null}`),
		[]byte(`{"success":true,"meta":{"currentPage":1,"lastPage":1},"data":[{"items":null}]}`),
		[]byte(`{"success":true,"meta":{"currentPage":2,"lastPage":2},"data":[]}`),
		[]byte(`{"success":true,"meta":{"currentPage":1,"nextPage":1,"lastPage":2},"data":[]}`),
		[]byte(`{"success":true,"meta":{"currentPage":1,"nextPage":null,"lastPage":2},"data":[]}`),
		[]byte(`{"success":true,"meta":{"currentPage":1,"nextPage":3,"lastPage":3},"data":[]}`),
		[]byte(`{"success":true,"meta":{"currentPage":2,"nextPage":3,"lastPage":2},"data":[]}`),
	} {
		if _, err := Timeline(body, "1234567", 1); err == nil {
			t.Fatalf("invalid timeline accepted: %s", body)
		}
	}
}

func TestSubjectsRejectsInvalidNumericOverviewValue(t *testing.T) {
	for _, value := range []string{"", "A", "6"} {
		body := []byte(`<a href="/grades/7654321/show?student=1234567"><strong class="d-block">Math</strong><div class="grades-cell-wrap"><div class="grade numeric">` + value + `</div></div></a>`)
		if _, err := Subjects(body, "1234567"); err == nil {
			t.Fatalf("numeric overview value %q accepted", value)
		}
	}
}
