package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/kryzhovnik/ednevnik/internal/model"
)

func Diff(old, current model.Snapshot) model.Changes {
	changes := model.Changes{SchemaVersion: model.SchemaVersion, ComparedAt: time.Now(), Items: []model.Change{}}
	if old.FetchedAt.IsZero() && len(old.Students) == 0 {
		return changes
	}
	knownGrades := map[string]bool{}
	knownAbsences := map[string]bool{}
	knownActivities := map[string]bool{}
	oldSubjects := map[string]string{}
	for _, student := range old.Students {
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
		for _, grade := range student.Grades {
			if !knownGrades[grade.ID] {
				changes.Items = append(changes.Items, model.Change{Kind: "grade_added", StudentID: grade.StudentID, RecordID: grade.ID, Summary: fmt.Sprintf("%s: %s", grade.Subject, grade.Value)})
			}
		}
		for _, absence := range student.Absences {
			if !knownAbsences[absence.ID] {
				changes.Items = append(changes.Items, model.Change{Kind: "absence_added", StudentID: absence.StudentID, RecordID: absence.ID, Summary: absence.Note})
			}
		}
		for _, activity := range student.Activities {
			if !knownActivities[activity.ID] {
				summary := activity.Title
				if activity.Note != "" {
					summary += ": " + activity.Note
				}
				changes.Items = append(changes.Items, model.Change{Kind: "activity_added", StudentID: activity.StudentID, RecordID: activity.ID, Summary: summary})
			}
		}
		for _, subject := range student.Subjects {
			key := student.Student.ID + ":" + subject.ID
			currentGrades := gradesKey(subject.DisplayedGrades)
			if previousGrades, exists := oldSubjects[key]; exists && previousGrades != currentGrades {
				changes.Items = append(changes.Items, model.Change{Kind: "subject_grades_changed", StudentID: student.Student.ID, RecordID: subject.ID, Summary: subject.Name})
			}
		}
	}
	return changes
}

func gradesKey(grades []string) string { raw, _ := json.Marshal(grades); return string(raw) }
