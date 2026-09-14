package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/kryzhovnik/ednevnik/internal/model"
)

func Diff(old, current model.Snapshot) model.Changes {
	changes := model.Changes{SchemaVersion: model.SchemaVersion, ComparedAt: time.Now(), From: old.FetchedAt, To: current.FetchedAt, Items: []model.Change{}}
	if old.FetchedAt.IsZero() && len(old.Students) == 0 {
		return changes
	}
	knownGrades := map[string]bool{}
	knownAbsences := map[string]bool{}
	knownActivities := map[string]bool{}
	knownStudents := map[string]bool{}
	oldSubjects := map[string]string{}
	for _, student := range old.Students {
		knownStudents[student.Student.ID] = true
		for _, grade := range student.Grades {
			knownGrades[grade.ID] = true
		}
		for _, absence := range student.Absences {
			knownAbsences[absence.ID] = true
		}
		for _, activity := range student.Activities {
			knownActivities[activity.ID] = true
		}
		for _, subject := range student.Subjects {
			oldSubjects[student.Student.ID+":"+subject.ID] = gradesKey(subject.DisplayedGrades)
		}
	}
	for _, student := range current.Students {
		if !knownStudents[student.Student.ID] {
			continue
		}
		for _, grade := range student.Grades {
			if !knownGrades[grade.ID] {
				changes.Items = append(changes.Items, model.Change{Kind: "grade_added", StudentID: grade.StudentID, StudentName: student.Student.Name, RecordID: grade.ID, Date: grade.Date, Subject: grade.Subject, Value: grade.Value, Note: grade.Note, Summary: fmt.Sprintf("%s: %s", grade.Subject, grade.Value)})
			}
		}
		for _, absence := range student.Absences {
			if !knownAbsences[absence.ID] {
				changes.Items = append(changes.Items, model.Change{Kind: "absence_added", StudentID: absence.StudentID, StudentName: student.Student.Name, RecordID: absence.ID, Date: absence.Date, Subject: absence.Subject, Period: absence.Period, Status: absence.Status, Note: absence.Note, Summary: absence.Note})
			}
		}
		for _, activity := range student.Activities {
			if !knownActivities[activity.ID] {
				summary := activity.Title
				if activity.Note != "" {
					summary += ": " + activity.Note
				}
				changes.Items = append(changes.Items, model.Change{Kind: "activity_added", StudentID: activity.StudentID, StudentName: student.Student.Name, RecordID: activity.ID, Date: activity.Date, Subject: activity.Title, Period: activity.Subtitle, Status: activity.TypeName, Value: activity.Symbol, Note: activity.Note, Summary: summary})
			}
		}
		for _, subject := range student.Subjects {
			key := student.Student.ID + ":" + subject.ID
			currentGrades := gradesKey(subject.DisplayedGrades)
			if previousGrades, exists := oldSubjects[key]; exists && previousGrades != currentGrades {
				changes.Items = append(changes.Items, model.Change{Kind: "subject_grades_changed", StudentID: student.Student.ID, StudentName: student.Student.Name, RecordID: subject.ID, Subject: subject.Name, Summary: subject.Name})
			}
		}
	}
	return changes
}

func gradesKey(grades []string) string { raw, _ := json.Marshal(grades); return string(raw) }
