package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kryzhovnik/ednevnik/internal/model"
)

type ReconcileOptions struct {
	Namespace            string
	AllowGradeRemovals   bool
	AllowAbsenceRemovals bool
}

// Diff is the schema-v2 compatibility entry point. It never infers removals.
func Diff(old, current model.Snapshot) model.Changes {
	return Reconcile(old, current, ReconcileOptions{Namespace: "legacy"})
}

// Reconcile prefers stable source identities. Without one, immutable source
// fields form a fallback group. Multiplicity is retained and uncertain pairing
// is reported as ambiguity instead of silently collapsing records.
func Reconcile(old, current model.Snapshot, opts ReconcileOptions) model.Changes {
	changes := model.Changes{SchemaVersion: model.SchemaVersion, ComparedAt: time.Now(), From: old.FetchedAt, To: current.FetchedAt, Items: []model.Change{}}
	if old.FetchedAt.IsZero() && len(old.Students) == 0 {
		return changes
	}
	oldStudents := make(map[string]model.StudentData, len(old.Students))
	for _, student := range old.Students {
		oldStudents[student.Student.ID] = student
	}
	for _, now := range current.Students {
		before, known := oldStudents[now.Student.ID]
		if !known {
			continue
		}
		changes.Items = append(changes.Items, reconcileGrades(opts, before, now)...)
		changes.Items = append(changes.Items, reconcileAbsences(opts, before, now)...)
		changes.Items = append(changes.Items, reconcileActivities(opts, before, now)...)
		changes.Items = append(changes.Items, reconcileOverview(opts, before, now)...)
	}
	return changes
}

type normalizedRecord struct {
	sourceID, recordID string
	fallbackOrdinal    int
	state              model.RecordState
}

func reconcileGrades(opts ReconcileOptions, old, now model.StudentData) []model.Change {
	a, b := map[string][]normalizedRecord{}, map[string][]normalizedRecord{}
	for _, v := range old.Grades {
		a[gradeAnchor(v)] = append(a[gradeAnchor(v)], normalizedGrade(v))
	}
	for _, v := range now.Grades {
		b[gradeAnchor(v)] = append(b[gradeAnchor(v)], normalizedGrade(v))
	}
	return reconcileGroups(opts, old.Student, "grade", a, b, opts.AllowGradeRemovals)
}

func reconcileAbsences(opts ReconcileOptions, old, now model.StudentData) []model.Change {
	a, b := map[string][]normalizedRecord{}, map[string][]normalizedRecord{}
	for _, v := range old.Absences {
		a[absenceAnchor(v)] = append(a[absenceAnchor(v)], normalizedAbsence(v))
	}
	for _, v := range now.Absences {
		b[absenceAnchor(v)] = append(b[absenceAnchor(v)], normalizedAbsence(v))
	}
	return reconcileGroups(opts, old.Student, "absence", a, b, opts.AllowAbsenceRemovals)
}

func reconcileGroups(opts ReconcileOptions, student model.Student, kind string, old, now map[string][]normalizedRecord, allowRemoval bool) []model.Change {
	var out []model.Change
	for _, anchor := range unionKeys(old, now) {
		originalOld, originalNow := old[anchor], now[anchor]
		a, b := cancelEqual(originalOld, originalNow)
		if len(a) == 0 && len(b) == 0 {
			continue
		}
		key := recordKey(opts.Namespace, student.ID, kind, anchor)
		switch {
		case len(a) == 1 && len(b) == 1 && (a[0].sourceID != "" || (len(originalOld) == 1 && len(originalNow) == 1)):
			out = append(out, makeChange(kind+"_updated", occurrenceKey(opts, student.ID, kind, anchor, a[0], b[0]), student, b[0].recordID, "detail", "record_correction", false, states(a), states(b)))
		case len(a) == 0:
			for i, item := range b {
				itemKey := key
				if item.sourceID == "" {
					itemKey = fallbackOccurrenceKey(opts, student.ID, kind, anchor, item, i+1)
				}
				out = append(out, makeChange(kind+"_added", itemKey, student, item.recordID, "detail", "new_record", false, nil, []model.RecordState{item.state}))
			}
		case len(b) == 0:
			if allowRemoval {
				for i, item := range a {
					itemKey := key
					if item.sourceID == "" {
						itemKey = fallbackOccurrenceKey(opts, student.ID, kind, anchor, item, i+1)
					}
					out = append(out, makeChange(kind+"_removed", itemKey, student, item.recordID, "detail", "record_removed", false, []model.RecordState{item.state}, nil))
				}
			}
		default:
			out = append(out, makeChange(kind+"_ambiguous", key, student, "", "detail", "identity_ambiguous", true, states(a), states(b)))
		}
	}
	return out
}

func cancelEqual(old, now []normalizedRecord) ([]normalizedRecord, []normalizedRecord) {
	used := make([]bool, len(now))
	remainingOld := make([]normalizedRecord, 0, len(old))
	for _, a := range old {
		matched := -1
		for i, b := range now {
			if used[i] || a.sourceID != b.sourceID || !equalState(a.state, b.state) {
				continue
			}
			matched = i
			break
		}
		if matched >= 0 {
			used[matched] = true
		} else {
			remainingOld = append(remainingOld, a)
		}
	}
	remainingNow := make([]normalizedRecord, 0, len(now))
	for i, b := range now {
		if !used[i] {
			remainingNow = append(remainingNow, b)
		}
	}
	return remainingOld, remainingNow
}

func reconcileActivities(opts ReconcileOptions, old, now model.StudentData) []model.Change {
	known := map[string]model.Activity{}
	for _, item := range old.Activities {
		known[activitySource(item)] = item
	}
	var out []model.Change
	for _, item := range now.Activities {
		sourceID := activitySource(item)
		key := recordKey(opts.Namespace, now.Student.ID, "activity", sourceID)
		before, ok := known[sourceID]
		afterState := activityState(item)
		if !ok {
			out = append(out, makeChange("activity_added", key, now.Student, item.ID, "timeline", "timeline_activity", false, nil, []model.RecordState{afterState}))
		} else if !equalState(activityState(before), afterState) {
			out = append(out, makeChange("activity_updated", key, now.Student, item.ID, "timeline", "timeline_activity_correction", false, []model.RecordState{activityState(before)}, []model.RecordState{afterState}))
		}
	}
	return out
}

func reconcileOverview(opts ReconcileOptions, old, now model.StudentData) []model.Change {
	known := map[string]model.Subject{}
	for _, subject := range old.Subjects {
		known[subject.ID] = subject
	}
	var out []model.Change
	for _, subject := range now.Subjects {
		before, ok := known[subject.ID]
		if !ok || gradesKey(before.DisplayedGrades) == gradesKey(subject.DisplayedGrades) {
			continue
		}
		out = append(out, model.Change{Kind: "subject_grades_changed", RecordKey: recordKey(opts.Namespace, now.Student.ID, "grade_overview", subject.ID), StudentID: now.Student.ID, StudentName: now.Student.Name, RecordID: subject.ID, Source: "grade_overview", Meaning: "displayed_grade_list_changed", Subject: subject.Name, Summary: subject.Name, Before: []model.RecordState{{Subject: before.Name, Values: append([]string(nil), before.DisplayedGrades...)}}, After: []model.RecordState{{Subject: subject.Name, Values: append([]string(nil), subject.DisplayedGrades...)}}})
	}
	return out
}

func makeChange(kind, key string, student model.Student, recordID, source, meaning string, ambiguous bool, before, after []model.RecordState) model.Change {
	c := model.Change{Kind: kind, RecordKey: key, StudentID: student.ID, StudentName: student.Name, RecordID: recordID, Source: source, Meaning: meaning, Ambiguous: ambiguous, Before: before, After: after}
	state := model.RecordState{}
	if len(after) == 1 {
		state = after[0]
	} else if len(before) == 1 {
		state = before[0]
	}
	c.Date, c.Subject, c.Period, c.Status, c.Value, c.Note = state.Date, state.Subject, state.Period, state.Status, state.Value, state.Note
	c.Summary = state.Subject
	if state.Value != "" {
		c.Summary = strings.TrimSpace(c.Summary + ": " + state.Value)
	}
	if state.Note != "" {
		c.Summary = strings.TrimSpace(c.Summary + ": " + state.Note)
	}
	if ambiguous {
		c.Summary = fmt.Sprintf("%s identity is ambiguous (%d prior, %d current)", kind, len(before), len(after))
	}
	return c
}

func normalizedGrade(v model.Grade) normalizedRecord {
	return normalizedRecord{v.SourceID, v.ID, v.FallbackOrdinal, model.RecordState{RecordID: v.ID, Date: v.Date, Subject: v.Subject, Value: v.Value, Note: v.Note}}
}
func normalizedAbsence(v model.Absence) normalizedRecord {
	return normalizedRecord{v.SourceID, v.ID, v.FallbackOrdinal, model.RecordState{RecordID: v.ID, Date: v.Date, Subject: v.Subject, Period: v.Period, Status: v.Status, Note: v.Note}}
}

func occurrenceKey(opts ReconcileOptions, enrolment, kind, anchor string, old, now normalizedRecord) string {
	if now.sourceID != "" {
		return recordKey(opts.Namespace, enrolment, kind, anchor)
	}
	ordinal := now.fallbackOrdinal
	if ordinal == 0 {
		ordinal = old.fallbackOrdinal
	}
	if ordinal == 0 {
		ordinal = 1
	}
	return recordKey(opts.Namespace, enrolment, kind, fmt.Sprintf("%s\x00occurrence:%d", anchor, ordinal))
}

func fallbackOccurrenceKey(opts ReconcileOptions, enrolment, kind, anchor string, record normalizedRecord, defaultOrdinal int) string {
	ordinal := record.fallbackOrdinal
	if ordinal == 0 {
		ordinal = defaultOrdinal
	}
	return recordKey(opts.Namespace, enrolment, kind, fmt.Sprintf("%s\x00occurrence:%d", anchor, ordinal))
}
func states(records []normalizedRecord) []model.RecordState {
	out := make([]model.RecordState, len(records))
	for i := range records {
		out[i] = records[i].state
	}
	return out
}
func equalState(a, b model.RecordState) bool {
	a.RecordID, b.RecordID = "", ""
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func gradeAnchor(v model.Grade) string {
	if v.SourceID != "" {
		return "source:" + v.SourceID
	}
	return strings.Join([]string{"fallback", v.SubjectID, v.Date, v.Kind}, "\x00")
}
func absenceAnchor(v model.Absence) string {
	if v.SourceID != "" {
		return "source:" + v.SourceID
	}
	return strings.Join([]string{"fallback", v.Subject, v.Date, v.Period}, "\x00")
}
func activitySource(v model.Activity) string {
	if v.PortalID <= 0 {
		return "legacy\x00" + v.ID
	}
	return fmt.Sprintf("%s\x00%d", v.Type, v.PortalID)
}
func activityState(v model.Activity) model.RecordState {
	return model.RecordState{RecordID: v.ID, Date: v.Date, Subject: v.Title, Period: v.Subtitle, Status: v.TypeName, Value: v.Symbol, Note: v.Note}
}
func recordKey(namespace, enrolment, kind, source string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{namespace, enrolment, kind, source}, "\x00")))
	return kind + ":" + hex.EncodeToString(sum[:12])
}
func gradesKey(grades []string) string { raw, _ := json.Marshal(grades); return string(raw) }
func unionKeys(a, b map[string][]normalizedRecord) []string {
	set := map[string]bool{}
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
