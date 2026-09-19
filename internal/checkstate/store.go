// Package checkstate owns the coherent account history document. Callers must
// hold the account coordination lease for the full load/commit operation.
package checkstate

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/kryzhovnik/ednevnik-cli/internal/model"
)

const SchemaVersion = 3

const (
	MaxDocumentBytes   = 16 << 20
	MaxEvents          = 10000
	MaxConsumers       = 32
	MaxIdentityEntries = 100000
)

var commitTestHook func(stage string) error

var ErrAbsent = errors.New("check state does not exist")
var ErrStorageLimit = errors.New("check state storage limit reached")

type InvalidError struct{ Err error }

func (e *InvalidError) Error() string { return "invalid check state: " + e.Err.Error() }
func (e *InvalidError) Unwrap() error { return e.Err }

// CommitError reports whether rename made the new generation visible. A
// caller must never write a compensating failure generation when Committed is
// true: later local status is the recovery interface for uncertain output.
type CommitError struct {
	Err       error
	Committed bool
}

func (e *CommitError) Error() string { return e.Err.Error() }
func (e *CommitError) Unwrap() error { return e.Err }

type Observation struct {
	EnrolmentID string            `json:"enrolment_id"`
	Snapshot    model.StudentData `json:"snapshot"`
	// TimelineBoundary holds source identities seen at the continuity seam.
	// Slice 06 owns how it proves and advances this boundary.
	TimelineBoundary       []string                     `json:"timeline_boundary"`
	UnresolvedFallbackKeys []string                     `json:"unresolved_fallback_keys,omitempty"`
	SeenSourceRecords      map[string]model.RecordState `json:"seen_source_records,omitempty"`
}

type Baseline struct {
	BaselineID             string                       `json:"baseline_id"`
	CheckID                string                       `json:"check_id"`
	Snapshot               model.StudentData            `json:"snapshot"`
	TimelineBoundary       []string                     `json:"timeline_boundary"`
	UnresolvedFallbackKeys []string                     `json:"unresolved_fallback_keys,omitempty"`
	SeenSourceRecords      map[string]model.RecordState `json:"seen_source_records,omitempty"`
}

type Document struct {
	SchemaVersion int                   `json:"schema_version"`
	Profile       model.CheckProfile    `json:"profile"`
	Generation    uint64                `json:"generation"`
	NextSequence  uint64                `json:"next_sequence"`
	Baselines     map[string]Baseline   `json:"baselines"`
	Events        []model.RetainedEvent `json:"events"`
	Consumers     map[string]Consumer   `json:"consumers"`
	LatestAttempt *model.CheckResult    `json:"latest_attempt"`
	LastSuccess   *model.CheckResult    `json:"last_success"`
}

type Consumer struct {
	AcknowledgedThrough uint64        `json:"acknowledged_through"`
	Pending             *PendingBatch `json:"pending,omitempty"`
	AcknowledgedTokens  []string      `json:"acknowledged_tokens,omitempty"`
}

type PendingBatch struct {
	Token   string `json:"token"`
	From    uint64 `json:"from_sequence"`
	Through uint64 `json:"through_sequence"`
}

type Batch struct {
	SchemaVersion int                   `json:"schema_version"`
	Consumer      string                `json:"consumer"`
	Token         string                `json:"ack_token,omitempty"`
	From          uint64                `json:"from_sequence,omitempty"`
	Through       uint64                `json:"through_sequence,omitempty"`
	Events        []model.RetainedEvent `json:"events"`
	More          bool                  `json:"more"`
}

type Store struct {
	Path        string
	LegacyPaths []string
}

func (s Store) Load(profile model.CheckProfile) (Document, error) {
	if legacy, err := legacyStateExists(s.LegacyPaths); err != nil {
		return Document{}, err
	} else if legacy {
		return Document{}, &InvalidError{Err: errors.New("schema-v2 state is present; its last-diff and per-consumer histories cannot be migrated without loss; move it aside after archiving it, then establish a schema-v3 baseline")}
	}
	info, statErr := os.Lstat(s.Path)
	if statErr == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0) {
		return Document{}, &InvalidError{Err: errors.New("state file must be a private regular file with mode 0600")}
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		return Document{}, statErr
	}
	f, err := os.Open(s.Path)
	if os.IsNotExist(err) {
		return newDocument(profile), ErrAbsent
	}
	if err != nil {
		return Document{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxDocumentBytes+1))
	if err != nil {
		return Document{}, err
	}
	if len(b) > MaxDocumentBytes {
		return Document{}, &InvalidError{Err: fmt.Errorf("%w: document exceeds %d bytes", ErrStorageLimit, MaxDocumentBytes)}
	}
	var d Document
	if err := json.Unmarshal(b, &d); err != nil {
		return Document{}, &InvalidError{Err: err}
	}
	// Consumers were added to schema v3. An absent map is the compatible empty
	// value; retained events and record identity maps remain untouched.
	if d.Consumers == nil {
		d.Consumers = map[string]Consumer{}
	}
	if err := validate(d, profile); err != nil {
		return Document{}, &InvalidError{Err: err}
	}
	return d, nil
}

func newDocument(profile model.CheckProfile) Document {
	return Document{SchemaVersion: SchemaVersion, Profile: profile, NextSequence: 1, Baselines: map[string]Baseline{}, Events: []model.RetainedEvent{}, Consumers: map[string]Consumer{}}
}

func validate(d Document, profile model.CheckProfile) error {
	if d.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema version %d", d.SchemaVersion)
	}
	if d.Profile != profile {
		return errors.New("profile or portal origin does not match")
	}
	if d.Generation == 0 || d.NextSequence == 0 || d.Baselines == nil || d.Events == nil || d.Consumers == nil || d.LatestAttempt == nil {
		return errors.New("incomplete state envelope")
	}
	if err := validateResult(*d.LatestAttempt, profile, false); err != nil {
		return fmt.Errorf("latest attempt: %w", err)
	}
	if d.LastSuccess != nil {
		if err := validateResult(*d.LastSuccess, profile, true); err != nil {
			return fmt.Errorf("last success: %w", err)
		}
	}
	for id, baseline := range d.Baselines {
		if id == "" || baseline.BaselineID == "" || baseline.CheckID == "" || baseline.Snapshot.Student.ID != id {
			return fmt.Errorf("invalid baseline for enrolment %q", id)
		}
		unresolved := map[string]bool{}
		for _, key := range baseline.UnresolvedFallbackKeys {
			if key == "" || unresolved[key] {
				return fmt.Errorf("invalid unresolved record identity for enrolment %q", id)
			}
			unresolved[key] = true
		}
		for key, state := range baseline.SeenSourceRecords {
			if key == "" || state.RecordID == "" {
				return fmt.Errorf("invalid seen source identity for enrolment %q", id)
			}
		}
	}
	last := uint64(0)
	expected := uint64(1)
	ids := map[string]bool{}
	for _, e := range d.Events {
		if e.ID == "" || e.Sequence != expected || e.Revision == 0 || e.CheckID == "" || ids[e.ID] || e.Change.Kind == "" || e.Change.StudentID == "" || (e.Change.RecordID == "" && e.Change.RecordKey == "") {
			return errors.New("invalid retained event ordering")
		}
		last, ids[e.ID] = e.Sequence, true
		expected++
	}
	if d.NextSequence != last+1 {
		return errors.New("next event sequence does not follow retained events")
	}
	if len(d.Events) > MaxEvents || len(d.Consumers) > MaxConsumers {
		return ErrStorageLimit
	}
	identityCount := 0
	for _, b := range d.Baselines {
		identityCount += len(b.UnresolvedFallbackKeys) + len(b.SeenSourceRecords)
	}
	if identityCount > MaxIdentityEntries {
		return ErrStorageLimit
	}
	for name, c := range d.Consumers {
		if name == "" || c.AcknowledgedThrough >= d.NextSequence {
			return errors.New("invalid consumer cursor")
		}
		if len(c.AcknowledgedTokens) > 1024 {
			return ErrStorageLimit
		}
		tokens := map[string]bool{}
		for _, token := range c.AcknowledgedTokens {
			if !validBatchToken(token) || tokens[token] {
				return errors.New("invalid acknowledged token")
			}
			tokens[token] = true
		}
		if c.Pending != nil && (!validBatchToken(c.Pending.Token) || tokens[c.Pending.Token] || c.Pending.From != c.AcknowledgedThrough+1 || c.Pending.Through < c.Pending.From || c.Pending.Through >= d.NextSequence) {
			return errors.New("invalid consumer pending batch")
		}
	}
	return nil
}

func validateResult(r model.CheckResult, profile model.CheckProfile, requireSuccess bool) error {
	if r.SchemaVersion != SchemaVersion || r.CheckID == "" || r.Profile != profile || r.Requested == nil || r.Coverage == nil || r.Baseline.NewEnrolments == nil || r.Changes.Items == nil {
		return errors.New("incomplete result envelope")
	}
	success := isSuccess(r.Outcome)
	if requireSuccess && !success {
		return errors.New("result is not successful")
	}
	if success && r.Baseline.ID == "" {
		return errors.New("successful result has no baseline")
	}
	if !success && r.Outcome != model.OutcomeIncomplete && r.Outcome != model.OutcomeFailed {
		return errors.New("unknown outcome")
	}
	if r.Outcome == model.OutcomeFailed && (r.Failure == nil || r.Failure.Reason == "") {
		return errors.New("failed result has no reason")
	}
	requested := map[string]bool{}
	for _, id := range r.Requested {
		if id == "" || requested[id] {
			return errors.New("invalid requested enrolments")
		}
		requested[id] = true
	}
	coverage := map[string]bool{}
	for _, c := range r.Coverage {
		if !requested[c.EnrolmentID] || coverage[c.EnrolmentID] {
			return errors.New("coverage is not bound to a requested enrolment")
		}
		coverage[c.EnrolmentID] = true
	}
	if r.Outcome != model.OutcomeFailed && len(coverage) != len(requested) {
		return errors.New("coverage does not contain every requested enrolment")
	}
	newIDs := map[string]bool{}
	for _, id := range r.Baseline.NewEnrolments {
		if !requested[id] || newIDs[id] {
			return errors.New("invalid new enrolments")
		}
		newIDs[id] = true
	}
	if r.Changes.Count != len(r.Changes.Items) {
		return errors.New("change count does not match items")
	}
	switch r.Outcome {
	case model.OutcomeInitialBaseline:
		if r.Changes.Count != 0 {
			return errors.New("initial baseline contains changes")
		}
	case model.OutcomeCompleteWithChanges:
		if r.Changes.Count == 0 {
			return errors.New("changed result has no changes")
		}
	case model.OutcomeCompleteWithoutChanges:
		if r.Changes.Count != 0 {
			return errors.New("unchanged result contains changes")
		}
	case model.OutcomeIncomplete:
		if r.Baseline.ID != "" || r.Baseline.PreviousID != "" || r.Changes.Count != 0 {
			return errors.New("incomplete result claims committed data")
		}
	case model.OutcomeFailed:
		if r.Baseline.ID != "" || r.Changes.Count != 0 {
			return errors.New("failed result claims committed data")
		}
	}
	return nil
}

func (s Store) Commit(profile model.CheckProfile, result model.CheckResult, observations []Observation) (Document, error) {
	d, err := s.Load(profile)
	if errors.Is(err, ErrAbsent) {
		d = newDocument(profile)
	} else if err != nil {
		return Document{}, err
	}
	d.Generation++
	resultCopy := result
	d.LatestAttempt = &resultCopy
	if isSuccess(result.Outcome) {
		if err := applySuccess(&d, &resultCopy, observations); err != nil {
			return Document{}, err
		}
		d.LatestAttempt = &resultCopy
		d.LastSuccess = &resultCopy
	}
	if err := validate(d, profile); err != nil {
		return Document{}, err
	}
	if err := writeAtomic(s.Path, d); err != nil {
		return Document{}, err
	}
	return d, nil
}

func (s Store) Register(profile model.CheckProfile, name, start string) (Document, error) {
	d, err := s.Load(profile)
	if err != nil {
		return Document{}, err
	}
	if _, exists := d.Consumers[name]; exists {
		return Document{}, errors.New("consumer already registered")
	}
	if name == "" {
		return Document{}, errors.New("consumer name is required")
	}
	if len(d.Consumers) >= MaxConsumers {
		return Document{}, ErrStorageLimit
	}
	var cursor uint64
	switch start {
	case "earliest":
		if len(d.Events) > 0 {
			cursor = d.Events[0].Sequence - 1
		}
	case "latest":
		cursor = d.NextSequence - 1
	default:
		return Document{}, errors.New("consumer start must be earliest or latest")
	}
	d.Consumers[name] = Consumer{AcknowledgedThrough: cursor}
	d.Generation++
	if err := validate(d, profile); err != nil {
		return Document{}, err
	}
	if err := writeAtomic(s.Path, d); err != nil {
		return Document{}, err
	}
	return d, nil
}

func (s Store) ReadBatch(profile model.CheckProfile, name string, limit int) (Batch, error) {
	if limit < 1 || limit > 1000 {
		return Batch{}, errors.New("batch limit must be between 1 and 1000")
	}
	d, err := s.Load(profile)
	if err != nil {
		return Batch{}, err
	}
	c, ok := d.Consumers[name]
	if !ok {
		return Batch{}, errors.New("consumer is not registered")
	}
	if c.Pending == nil {
		items := eventsAfter(d.Events, c.AcknowledgedThrough, limit)
		if len(items) == 0 {
			return Batch{SchemaVersion: SchemaVersion, Consumer: name, Events: []model.RetainedEvent{}}, nil
		}
		token, err := newBatchToken()
		if err != nil {
			return Batch{}, err
		}
		c.Pending = &PendingBatch{Token: token, From: items[0].Sequence, Through: items[len(items)-1].Sequence}
		d.Consumers[name] = c
		d.Generation++
		if err := validate(d, profile); err != nil {
			return Batch{}, err
		}
		if err := writeAtomic(s.Path, d); err != nil {
			return Batch{}, err
		}
	}
	items := eventsRange(d.Events, c.Pending.From, c.Pending.Through)
	if len(items) == 0 || items[0].Sequence != c.Pending.From || items[len(items)-1].Sequence != c.Pending.Through {
		return Batch{}, &InvalidError{Err: errors.New("pending batch events are missing")}
	}
	return Batch{SchemaVersion: SchemaVersion, Consumer: name, Token: c.Pending.Token, From: c.Pending.From, Through: c.Pending.Through, Events: items, More: c.Pending.Through < d.NextSequence-1}, nil
}

func (s Store) Acknowledge(profile model.CheckProfile, name, token string) (Document, error) {
	d, err := s.Load(profile)
	if err != nil {
		return Document{}, err
	}
	c, ok := d.Consumers[name]
	if !ok {
		return Document{}, errors.New("consumer is not registered")
	}
	if token == "" {
		return Document{}, errors.New("ack token is required")
	}
	for _, acknowledged := range c.AcknowledgedTokens {
		if token == acknowledged {
			return d, nil
		}
	}
	if c.Pending == nil || token != c.Pending.Token {
		return Document{}, errors.New("ack token was not delivered to this consumer")
	}
	c.AcknowledgedThrough = c.Pending.Through
	c.AcknowledgedTokens = append(c.AcknowledgedTokens, token)
	if len(c.AcknowledgedTokens) > 1024 {
		return Document{}, ErrStorageLimit
	}
	c.Pending = nil
	d.Consumers[name] = c
	d.Generation++
	if err := validate(d, profile); err != nil {
		return Document{}, err
	}
	if err := writeAtomic(s.Path, d); err != nil {
		return Document{}, err
	}
	return d, nil
}

func eventsAfter(events []model.RetainedEvent, cursor uint64, limit int) []model.RetainedEvent {
	out := make([]model.RetainedEvent, 0, limit)
	for _, event := range events {
		if event.Sequence > cursor {
			out = append(out, event)
			if len(out) == limit {
				break
			}
		}
	}
	return out
}
func eventsRange(events []model.RetainedEvent, from, through uint64) []model.RetainedEvent {
	out := []model.RetainedEvent{}
	for _, event := range events {
		if event.Sequence >= from && event.Sequence <= through {
			out = append(out, event)
		}
	}
	return out
}
func newBatchToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func validBatchToken(token string) bool {
	b, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(b) == 24
}

func isSuccess(outcome string) bool {
	return outcome == model.OutcomeInitialBaseline || outcome == model.OutcomeCompleteWithChanges || outcome == model.OutcomeCompleteWithoutChanges
}

func applySuccess(d *Document, result *model.CheckResult, observations []Observation) error {
	byID := make(map[string]Observation, len(observations))
	for _, o := range observations {
		if o.EnrolmentID == "" || byID[o.EnrolmentID].EnrolmentID != "" {
			return errors.New("invalid or duplicate observation")
		}
		byID[o.EnrolmentID] = o
	}
	for _, id := range result.Requested {
		o, ok := byID[id]
		if !ok {
			return fmt.Errorf("missing observation for enrolment %s", id)
		}
		if o.Snapshot.Student.ID != id {
			return fmt.Errorf("observation identity mismatch for enrolment %s", id)
		}
		d.Baselines[id] = Baseline{BaselineID: result.Baseline.ID, CheckID: result.CheckID, Snapshot: o.Snapshot, TimelineBoundary: append([]string(nil), o.TimelineBoundary...), UnresolvedFallbackKeys: append([]string(nil), o.UnresolvedFallbackKeys...), SeenSourceRecords: cloneRecordStates(o.SeenSourceRecords)}
	}
	if len(byID) != len(result.Requested) {
		return errors.New("observation contains an unrequested enrolment")
	}
	if result.Outcome == model.OutcomeInitialBaseline || len(result.Baseline.NewEnrolments) > 0 {
		// Existing records for a newly monitored enrolment establish state only.
		newSet := map[string]bool{}
		for _, id := range result.Baseline.NewEnrolments {
			newSet[id] = true
		}
		for _, c := range result.Changes.Items {
			if newSet[c.StudentID] {
				return fmt.Errorf("new enrolment %s generated a historical event", c.StudentID)
			}
		}
	}
	for i := range result.Changes.Items {
		c := result.Changes.Items[i]
		id := fmt.Sprintf("event_%s_%d", strings.TrimPrefix(result.CheckID, "check_"), i+1)
		redelivered := false
		for _, event := range d.Events {
			if event.ID != id {
				continue
			}
			if event.CheckID != result.CheckID || !reflect.DeepEqual(event.Change, c) {
				return fmt.Errorf("event identity %s conflicts with retained transition", id)
			}
			redelivered = true
			break
		}
		if redelivered {
			continue
		}
		revision := uint64(1)
		for j := len(d.Events) - 1; j >= 0; j-- {
			if sameRecord(d.Events[j].Change, c) {
				revision = d.Events[j].Revision + 1
				break
			}
		}
		d.Events = append(d.Events, model.RetainedEvent{ID: id, Sequence: d.NextSequence, Revision: revision, CheckID: result.CheckID, Change: c})
		d.NextSequence++
	}
	result.Changes.Count = len(result.Changes.Items)
	return nil
}

func cloneRecordStates(in map[string]model.RecordState) map[string]model.RecordState {
	out := make(map[string]model.RecordState, len(in))
	for key, state := range in {
		out[key] = state
	}
	return out
}

func sameRecord(a, b model.Change) bool {
	if a.RecordKey != "" && b.RecordKey != "" {
		return a.StudentID == b.StudentID && a.RecordKey == b.RecordKey
	}
	// Early schema-v3 events predate RecordKey. Recover lineage only from source
	// fields that were already immutable enough to be unambiguous. Event IDs are
	// never rewritten; grade fallback lineage remains unrecoverable because the
	// old event did not retain subject ID, assessment kind, or multiplicity.
	if a.StudentID != b.StudentID || recordFamily(a.Kind) != recordFamily(b.Kind) {
		return false
	}
	switch recordFamily(a.Kind) {
	case "absence":
		return a.Subject != "" && a.Date != "" && a.Period != "" && a.Subject == b.Subject && a.Date == b.Date && a.Period == b.Period
	case "activity":
		return a.RecordID != "" && a.RecordID == b.RecordID
	case "subject_grades":
		return a.RecordID != "" && a.RecordID == b.RecordID
	}
	return a.StudentID == b.StudentID && a.Kind == b.Kind && a.RecordID == b.RecordID
}

func recordFamily(kind string) string {
	for _, suffix := range []string{"_added", "_updated", "_removed", "_ambiguous"} {
		if strings.HasSuffix(kind, suffix) {
			return strings.TrimSuffix(kind, suffix)
		}
	}
	if kind == "subject_grades_changed" {
		return "subject_grades"
	}
	return kind
}

func legacyStateExists(paths []string) (bool, error) {
	for _, path := range paths {
		if path == "" {
			continue
		}
		_, err := os.Stat(path)
		if err == nil {
			return true, nil
		}
		if !os.IsNotExist(err) {
			return false, err
		}
	}
	return false, nil
}

func writeAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return &CommitError{Err: err}
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return &CommitError{Err: err}
	}
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return &CommitError{Err: err}
	}
	b = append(b, '\n')
	if len(b) > MaxDocumentBytes {
		return &CommitError{Err: fmt.Errorf("%w: document exceeds %d bytes", ErrStorageLimit, MaxDocumentBytes)}
	}
	name := filepath.Base(path) + ".tmp-" + randomSuffix()
	tmp := filepath.Join(filepath.Dir(path), name)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return &CommitError{Err: err}
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = io.Copy(f, strings.NewReader(string(b))); err != nil {
		return &CommitError{Err: err}
	}
	if err = f.Sync(); err != nil {
		return &CommitError{Err: err}
	}
	if err = f.Close(); err != nil {
		return &CommitError{Err: err}
	}
	if commitTestHook != nil {
		if err = commitTestHook("before_rename"); err != nil {
			return &CommitError{Err: err}
		}
	}
	if err = os.Rename(tmp, path); err != nil {
		return &CommitError{Err: err}
	}
	ok = true
	if commitTestHook != nil {
		if err = commitTestHook("after_rename"); err != nil {
			return &CommitError{Err: err, Committed: true}
		}
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return &CommitError{Err: err, Committed: true}
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil {
		return &CommitError{Err: err, Committed: true}
	}
	if closeErr != nil {
		return &CommitError{Err: closeErr, Committed: true}
	}
	return nil
}

func randomSuffix() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b)
}

func Status(d Document) model.CheckStatus {
	return model.CheckStatus{SchemaVersion: SchemaVersion, History: "retained_events", LatestAttempt: d.LatestAttempt, LastSuccess: d.LastSuccess}
}

func SortedBaselineIDs(d Document) []string {
	ids := make([]string, 0, len(d.Baselines))
	for id := range d.Baselines {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
