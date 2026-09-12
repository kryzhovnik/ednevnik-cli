package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/kryzhovnik/ednevnik/internal/model"
)

func Diff(old, current model.Snapshot) model.Changes {
	knownGrades := map[string]bool{}
	knownAbsences := map[string]bool{}
	oldSubjects := map[string]string{}
	for _, student := range old.Students {
		for _, grade := range student.Grades {
			knownGrades[grade.ID] = true
		}
		for _, absence := range student.Absences {
			knownAbsences[absence.ID] = true
		}
		for _, subject := range student.Subjects {
			oldSubjects[student.Student.ID+":"+subject.ID] = gradesKey(subject.DisplayedGrades)
		}
	}
	changes := model.Changes{SchemaVersion: model.SchemaVersion, ComparedAt: time.Now(), Items: []model.Change{}}
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
