package store

import (
	"testing"
	"time"

	"github.com/kryzhovnik/ednevnik-cli/internal/model"
)

func snapshots(before, after model.StudentData) (model.Snapshot, model.Snapshot) {
	return model.Snapshot{FetchedAt: time.Unix(1, 0), Students: []model.StudentData{before}}, model.Snapshot{FetchedAt: time.Unix(2, 0), Students: []model.StudentData{after}}
}

func TestReconcileStableIdentityCarriesBeforeAndAfter(t *testing.T) {
	student := model.Student{ID: "enrolment-1", Name: "Student"}
	before, after := snapshots(
		model.StudentData{Student: student, Absences: []model.Absence{{ID: "old-content-hash", SourceID: "absence-9", StudentID: student.ID, Subject: "Math", Date: "date", Period: "2", Status: "unexcused", Note: "old"}}},
		model.StudentData{Student: student, Absences: []model.Absence{{ID: "new-content-hash", SourceID: "absence-9", StudentID: student.ID, Subject: "Math", Date: "date", Period: "2", Status: "excused", Note: "fixed"}}},
	)
	changes := Reconcile(before, after, ReconcileOptions{Namespace: "family"}).Items
	if len(changes) != 1 || changes[0].Kind != "absence_updated" || changes[0].RecordKey == "" {
		t.Fatalf("changes = %#v", changes)
	}
	if changes[0].Before[0].Status != "unexcused" || changes[0].After[0].Status != "excused" {
		t.Fatalf("context = %#v -> %#v", changes[0].Before, changes[0].After)
	}
}

func TestReconcileMigratesOldContentHashFallbackWithoutAddition(t *testing.T) {
	student := model.Student{ID: "enrolment-1"}
	before, after := snapshots(
		model.StudentData{Student: student, Absences: []model.Absence{{ID: "old-content-hash", StudentID: student.ID, Subject: "Math", Date: "date", Period: "2", Status: "unexcused", Note: "old"}}},
		model.StudentData{Student: student, Absences: []model.Absence{{ID: "new-parser-id", StudentID: student.ID, Subject: "Math", Date: "date", Period: "2", Status: "excused", Note: "fixed"}}},
	)
	changes := Reconcile(before, after, ReconcileOptions{Namespace: "family"}).Items
	if len(changes) != 1 || changes[0].Kind != "absence_updated" {
		t.Fatalf("changes = %#v", changes)
	}
}

func TestReconcilePreservesEqualLookingMultiplicityAndAmbiguity(t *testing.T) {
	student := model.Student{ID: "enrolment-1"}
	old := []model.Grade{
		{ID: "old-1", StudentID: student.ID, SubjectID: "math", Subject: "Math", Date: "date", Kind: "oral", Value: "4"},
		{ID: "old-2", StudentID: student.ID, SubjectID: "math", Subject: "Math", Date: "date", Kind: "oral", Value: "4"},
	}
	now := []model.Grade{
		{ID: "new-1", StudentID: student.ID, SubjectID: "math", Subject: "Math", Date: "date", Kind: "oral", Value: "5"},
		{ID: "new-2", StudentID: student.ID, SubjectID: "math", Subject: "Math", Date: "date", Kind: "oral", Value: "3"},
	}
	before, after := snapshots(model.StudentData{Student: student, Grades: old}, model.StudentData{Student: student, Grades: now})
	changes := Reconcile(before, after, ReconcileOptions{Namespace: "family"}).Items
	if len(changes) != 1 || !changes[0].Ambiguous || changes[0].Kind != "grade_ambiguous" || len(changes[0].Before) != 2 || len(changes[0].After) != 2 {
		t.Fatalf("changes = %#v", changes)
	}
}

func TestReconcileDoesNotInferWhichEqualRecordChanged(t *testing.T) {
	student := model.Student{ID: "enrolment-1"}
	old := []model.Grade{
		{ID: "old-1", FallbackOrdinal: 1, StudentID: student.ID, SubjectID: "math", Subject: "Math", Date: "date", Kind: "oral", Value: "4"},
		{ID: "old-2", FallbackOrdinal: 2, StudentID: student.ID, SubjectID: "math", Subject: "Math", Date: "date", Kind: "oral", Value: "4"},
	}
	now := []model.Grade{
		{ID: "new-1", FallbackOrdinal: 1, StudentID: student.ID, SubjectID: "math", Subject: "Math", Date: "date", Kind: "oral", Value: "4"},
		{ID: "new-2", FallbackOrdinal: 2, StudentID: student.ID, SubjectID: "math", Subject: "Math", Date: "date", Kind: "oral", Value: "5"},
	}
	before, after := snapshots(model.StudentData{Student: student, Grades: old}, model.StudentData{Student: student, Grades: now})
	changes := Reconcile(before, after, ReconcileOptions{Namespace: "family"}).Items
	if len(changes) != 1 || !changes[0].Ambiguous || len(changes[0].Before) != 1 || len(changes[0].After) != 1 {
		t.Fatalf("changes = %#v", changes)
	}
}

func TestReconcileFallbackAdditionAndCorrectionShareRecordKey(t *testing.T) {
	student := model.Student{ID: "enrolment-1"}
	empty := model.StudentData{Student: student}
	red := model.StudentData{Student: student, Absences: []model.Absence{{ID: "fallback", FallbackOrdinal: 1, StudentID: student.ID, Subject: "Math", Date: "date", Period: "2", Status: "unexcused"}}}
	green := model.StudentData{Student: student, Absences: []model.Absence{{ID: "fallback", FallbackOrdinal: 1, StudentID: student.ID, Subject: "Math", Date: "date", Period: "2", Status: "excused"}}}
	a, b := snapshots(empty, red)
	added := Reconcile(a, b, ReconcileOptions{Namespace: "family"}).Items
	b, c := snapshots(red, green)
	updated := Reconcile(b, c, ReconcileOptions{Namespace: "family"}).Items
	if len(added) != 1 || len(updated) != 1 || added[0].RecordKey != updated[0].RecordKey {
		t.Fatalf("added=%#v updated=%#v", added, updated)
	}
}

func TestReconcileFallbackInsertionDoesNotReuseExistingOccurrenceKey(t *testing.T) {
	student := model.Student{ID: "enrolment-1"}
	empty := model.StudentData{Student: student}
	a := model.Absence{ID: "a", FallbackOrdinal: 1, StudentID: student.ID, Subject: "Math", Date: "date", Period: "2", Status: "unexcused", Note: "A"}
	b := model.Absence{ID: "b", FallbackOrdinal: 1, StudentID: student.ID, Subject: "Math", Date: "date", Period: "2", Status: "excused", Note: "B"}
	aMoved := a
	aMoved.FallbackOrdinal = 2
	before, after := snapshots(empty, model.StudentData{Student: student, Absences: []model.Absence{a}})
	added := Reconcile(before, after, ReconcileOptions{Namespace: "family"}).Items
	before, after = snapshots(model.StudentData{Student: student, Absences: []model.Absence{a}}, model.StudentData{Student: student, Absences: []model.Absence{b, aMoved}})
	result := ReconcileWithState(before, after, ReconcileOptions{Namespace: "family"})
	changed := result.Changes.Items
	if len(added) != 1 || len(changed) != 1 || !changed[0].Ambiguous || changed[0].Kind != "absence_ambiguous" || added[0].RecordKey == changed[0].RecordKey || len(changed[0].Before) != 1 || len(changed[0].After) != 2 || len(result.UnresolvedFallbackKeys) != 1 {
		t.Fatalf("added=%#v result=%#v", added, result)
	}
	bOnly := model.StudentData{Student: student, Absences: []model.Absence{b}}
	before, after = snapshots(model.StudentData{Student: student, Absences: []model.Absence{b, aMoved}}, bOnly)
	result = ReconcileWithState(before, after, ReconcileOptions{Namespace: "family", UnresolvedFallbackKeys: result.UnresolvedFallbackKeys})
	if len(result.Changes.Items) != 1 || !result.Changes.Items[0].Ambiguous {
		t.Fatalf("contraction=%#v", result)
	}
	bCorrected := b
	bCorrected.Status = "unexcused"
	before, after = snapshots(bOnly, model.StudentData{Student: student, Absences: []model.Absence{bCorrected}})
	result = ReconcileWithState(before, after, ReconcileOptions{Namespace: "family", UnresolvedFallbackKeys: result.UnresolvedFallbackKeys})
	if len(result.Changes.Items) != 1 || !result.Changes.Items[0].Ambiguous || result.Changes.Items[0].RecordKey != changed[0].RecordKey {
		t.Fatalf("correction=%#v", result)
	}
	before, after = snapshots(model.StudentData{Student: student, Absences: []model.Absence{bCorrected}}, empty)
	result = ReconcileWithState(before, after, ReconcileOptions{Namespace: "family", UnresolvedFallbackKeys: result.UnresolvedFallbackKeys})
	if len(result.UnresolvedFallbackKeys) != 1 {
		t.Fatalf("disappearance cleared uncertainty: %#v", result)
	}
	before, after = snapshots(empty, bOnly)
	result = ReconcileWithState(before, after, ReconcileOptions{Namespace: "family", UnresolvedFallbackKeys: result.UnresolvedFallbackKeys})
	if len(result.Changes.Items) != 1 || !result.Changes.Items[0].Ambiguous {
		t.Fatalf("reappearance=%#v", result)
	}
}

func TestReconcileFallbackDisappearanceMakesReappearanceAmbiguous(t *testing.T) {
	student := model.Student{ID: "enrolment-1"}
	empty := model.StudentData{Student: student}
	a := model.Absence{ID: "a", FallbackOrdinal: 1, StudentID: student.ID, Subject: "Math", Date: "date", Period: "2", Status: "unexcused", Note: "A"}
	b := model.Absence{ID: "b", FallbackOrdinal: 1, StudentID: student.ID, Subject: "Math", Date: "date", Period: "2", Status: "excused", Note: "B"}
	before, after := snapshots(model.StudentData{Student: student, Absences: []model.Absence{a}}, empty)
	disappeared := ReconcileWithState(before, after, ReconcileOptions{Namespace: "family"})
	if len(disappeared.Changes.Items) != 0 || len(disappeared.UnresolvedFallbackKeys) != 1 {
		t.Fatalf("disappeared=%#v", disappeared)
	}
	before, after = snapshots(empty, model.StudentData{Student: student, Absences: []model.Absence{b}})
	reappeared := ReconcileWithState(before, after, ReconcileOptions{Namespace: "family", UnresolvedFallbackKeys: disappeared.UnresolvedFallbackKeys})
	if len(reappeared.Changes.Items) != 1 || !reappeared.Changes.Items[0].Ambiguous || reappeared.Changes.Items[0].Kind != "absence_ambiguous" {
		t.Fatalf("reappeared=%#v", reappeared)
	}
}

func TestReconcileStableSourceReappearanceUsesKnownIdentity(t *testing.T) {
	student := model.Student{ID: "enrolment-1"}
	known := model.Absence{ID: "source-42", SourceID: "source-42", StudentID: student.ID, Subject: "Math", Date: "date", Period: "2", Status: "excused"}
	current := model.StudentData{Student: student, Absences: []model.Absence{known}}
	initial := ReconcileWithState(model.Snapshot{}, model.Snapshot{Students: []model.StudentData{current}}, ReconcileOptions{Namespace: "family"})
	if len(initial.Changes.Items) != 0 || len(initial.SeenSourceRecords) != 1 {
		t.Fatalf("initial=%#v", initial)
	}
	before, after := snapshots(current, model.StudentData{Student: student})
	missing := ReconcileWithState(before, after, ReconcileOptions{Namespace: "family", SeenSourceRecords: initial.SeenSourceRecords})
	before, after = snapshots(model.StudentData{Student: student}, current)
	returned := ReconcileWithState(before, after, ReconcileOptions{Namespace: "family", SeenSourceRecords: missing.SeenSourceRecords})
	if len(returned.Changes.Items) != 1 || returned.Changes.Items[0].Kind != "absence_reappeared" || returned.Changes.Items[0].Meaning != "observed_reappearance" || len(returned.Changes.Items[0].Before) != 1 || len(returned.Changes.Items[0].After) != 1 {
		t.Fatalf("returned=%#v", returned)
	}
}

func TestReconcileEqualLookingAdditionKeepsBothOccurrences(t *testing.T) {
	student := model.Student{ID: "enrolment-1"}
	grade := model.Grade{StudentID: student.ID, SubjectID: "math", Subject: "Math", Date: "date", Kind: "oral", Value: "5"}
	before, after := snapshots(model.StudentData{Student: student}, model.StudentData{Student: student, Grades: []model.Grade{grade, grade}})
	changes := Reconcile(before, after, ReconcileOptions{Namespace: "family"}).Items
	if len(changes) != 2 || changes[0].RecordKey == changes[1].RecordKey {
		t.Fatalf("changes = %#v", changes)
	}
}

func TestReconcileDoesNotInferFeedOrCurrentListRemoval(t *testing.T) {
	student := model.Student{ID: "enrolment-1"}
	before, after := snapshots(
		model.StudentData{Student: student, Activities: []model.Activity{{ID: "a", StudentID: student.ID, PortalID: 7, Type: "observation"}}, Absences: []model.Absence{{ID: "x", StudentID: student.ID, Subject: "Math", Date: "date", Period: "2"}}},
		model.StudentData{Student: student},
	)
	if changes := Reconcile(before, after, ReconcileOptions{Namespace: "family"}).Items; len(changes) != 0 {
		t.Fatalf("changes = %#v", changes)
	}
}

func TestReconcileLabelsTimelineAndOverviewSignalsSeparately(t *testing.T) {
	student := model.Student{ID: "enrolment-1"}
	before, after := snapshots(
		model.StudentData{Student: student, Subjects: []model.Subject{{ID: "math", Name: "Math", DisplayedGrades: []string{"4"}}}, Activities: []model.Activity{{ID: "a", StudentID: student.ID, PortalID: 7, Type: "grade", Title: "Math", Symbol: "4"}}},
		model.StudentData{Student: student, Subjects: []model.Subject{{ID: "math", Name: "Math", DisplayedGrades: []string{"4", "5"}}}, Activities: []model.Activity{{ID: "a", StudentID: student.ID, PortalID: 7, Type: "grade", Title: "Math", Symbol: "5"}}},
	)
	changes := Reconcile(before, after, ReconcileOptions{Namespace: "family"}).Items
	if len(changes) != 2 || changes[0].Source != "timeline" || changes[1].Source != "grade_overview" || changes[0].RecordKey == changes[1].RecordKey {
		t.Fatalf("changes = %#v", changes)
	}
}

func TestReconcileRecordKeyIsAccountNamespaced(t *testing.T) {
	student := model.Student{ID: "enrolment-1"}
	before, after := snapshots(model.StudentData{Student: student}, model.StudentData{Student: student, Activities: []model.Activity{{ID: "a", StudentID: student.ID, PortalID: 7, Type: "note", Title: "Note"}}})
	a := Reconcile(before, after, ReconcileOptions{Namespace: "family-a"}).Items[0].RecordKey
	b := Reconcile(before, after, ReconcileOptions{Namespace: "family-b"}).Items[0].RecordKey
	if a == b {
		t.Fatalf("keys are not account namespaced: %q", a)
	}
}
