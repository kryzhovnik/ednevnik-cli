package store

import (
	"testing"
	"time"

	"github.com/kryzhovnik/ednevnik/internal/model"
)

func TestDiffReturnsOnlyNewRecords(t *testing.T) {
	old := model.Snapshot{FetchedAt: time.Now(), Students: []model.StudentData{{Grades: []model.Grade{{ID: "old"}}}}}
	current := model.Snapshot{Students: []model.StudentData{{Grades: []model.Grade{{ID: "old"}, {ID: "new", StudentID: "s1", Subject: "Math", Value: "5"}}}}}
	changes := Diff(old, current)
	if len(changes.Items) != 1 || changes.Items[0].RecordID != "new" {
		t.Fatalf("changes = %#v", changes)
	}
}

func TestDiffDetectsDisplayedGradeChange(t *testing.T) {
	old := model.Snapshot{FetchedAt: time.Now(), Students: []model.StudentData{{Student: model.Student{ID: "s1"}, Subjects: []model.Subject{{ID: "math", Name: "Math", DisplayedGrades: []string{"4"}}}}}}
	current := model.Snapshot{Students: []model.StudentData{{Student: model.Student{ID: "s1"}, Subjects: []model.Subject{{ID: "math", Name: "Math", DisplayedGrades: []string{"4", "5"}}}}}}
	changes := Diff(old, current)
	if len(changes.Items) != 1 || changes.Items[0].Kind != "subject_grades_changed" {
		t.Fatalf("changes = %#v", changes)
	}
}

func TestDiffDoesNotReportExistingRecordsOnFirstSync(t *testing.T) {
	current := model.Snapshot{Students: []model.StudentData{{Activities: []model.Activity{{ID: "activity"}}, Absences: []model.Absence{{ID: "absence"}}}}}
	changes := Diff(model.Snapshot{}, current)
	if len(changes.Items) != 0 {
		t.Fatalf("changes = %#v", changes)
	}
}

func TestDiffDetectsNewActivity(t *testing.T) {
	old := model.Snapshot{FetchedAt: time.Now(), Students: []model.StudentData{{Activities: []model.Activity{{ID: "old"}}}}}
	current := model.Snapshot{Students: []model.StudentData{{Activities: []model.Activity{{ID: "old"}, {ID: "new", StudentID: "s1", Title: "Math", Note: "Observation"}}}}}
	changes := Diff(old, current)
	if len(changes.Items) != 1 || changes.Items[0].Kind != "activity_added" || changes.Items[0].Summary != "Math: Observation" {
		t.Fatalf("changes = %#v", changes)
	}
}

func TestDiffIncludesStudentAndRecordDetails(t *testing.T) {
	old := model.Snapshot{FetchedAt: time.Now(), Students: []model.StudentData{{Student: model.Student{ID: "s1", Name: "Mark"}}}}
	current := model.Snapshot{Students: []model.StudentData{{Student: model.Student{ID: "s1", Name: "Mark"}, Absences: []model.Absence{{ID: "new", StudentID: "s1", Subject: "French", Date: "08. 09. 2026.", Period: "2. Час", Status: "unexcused", Note: "Bonjour"}}}}}
	changes := Diff(old, current)
	if len(changes.Items) != 1 {
		t.Fatalf("changes = %#v", changes)
	}
	change := changes.Items[0]
	if change.StudentName != "Mark" || change.Subject != "French" || change.Date != "08. 09. 2026." || change.Period != "2. Час" || change.Status != "unexcused" {
		t.Fatalf("change = %#v", change)
	}
}

func TestDiffDoesNotReportHistoryForNewEnrollment(t *testing.T) {
	old := model.Snapshot{FetchedAt: time.Now(), Students: []model.StudentData{{Student: model.Student{ID: "s1", Name: "Mark"}}}}
	current := model.Snapshot{Students: []model.StudentData{
		{Student: model.Student{ID: "s1", Name: "Mark"}},
		{Student: model.Student{ID: "s2", Name: "Mary"}, Absences: []model.Absence{{ID: "history", StudentID: "s2"}}, Activities: []model.Activity{{ID: "history", StudentID: "s2"}}},
	}}
	changes := Diff(old, current)
	if len(changes.Items) != 0 {
		t.Fatalf("changes = %#v", changes)
	}
}
