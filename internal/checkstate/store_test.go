package checkstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kryzhovnik/ednevnik/internal/model"
)

func result(profile model.CheckProfile, id, outcome string, requested ...string) model.CheckResult {
	coverage := make([]model.EnrolmentCoverage, 0, len(requested))
	for _, enrolment := range requested {
		coverage = append(coverage, model.EnrolmentCoverage{EnrolmentID: enrolment, Sections: []model.SectionCoverage{}, Continuity: model.ContinuityCoverage{State: "complete"}})
	}
	baseline := ""
	if isSuccess(outcome) {
		baseline = "baseline_" + id
	}
	return model.CheckResult{SchemaVersion: 3, CheckID: id, Outcome: outcome, Profile: profile, Requested: requested, Coverage: coverage, StartedAt: time.Now(), CompletedAt: time.Now(), Baseline: model.BaselineReference{ID: baseline, NewEnrolments: []string{}}, Changes: model.ChangeSummary{Items: []model.Change{}}, Guidance: model.Guidance{}}
}
func observations(ids ...string) []Observation {
	out := []Observation{}
	for _, id := range ids {
		out = append(out, Observation{EnrolmentID: id, Snapshot: model.StudentData{Student: model.Student{ID: id}}, TimelineBoundary: []string{"activity-1"}})
	}
	return out
}

func TestGenerationCommitIsAtomicAndRetainsEvents(t *testing.T) {
	profile := model.CheckProfile{ID: "family", Origin: "https://portal.example"}
	path := filepath.Join(t.TempDir(), "check-state.json")
	s := Store{Path: path}
	first := result(profile, "check_a", model.OutcomeInitialBaseline, "1111111", "2222222")
	first.Baseline.NewEnrolments = []string{"1111111", "2222222"}
	if _, err := s.Commit(profile, first, observations("1111111", "2222222")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second := result(profile, "check_b", model.OutcomeCompleteWithChanges, "1111111")
	second.Changes = model.ChangeSummary{Count: 1, Items: []model.Change{{Kind: "grade_update", StudentID: "1111111", RecordID: "g1", Summary: "changed"}}}
	commitTestHook = func(stage string) error {
		if stage == "before_rename" {
			return errors.New("injected storage exhaustion")
		}
		return nil
	}
	defer func() { commitTestHook = nil }()
	if _, err := s.Commit(profile, second, observations("1111111")); err == nil {
		t.Fatal("faulted commit succeeded")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed commit exposed a partial generation")
	}
	commitTestHook = nil
	d, err := s.Commit(profile, second, observations("1111111"))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Events) != 1 || d.Events[0].Sequence != 1 || d.Events[0].Revision != 1 || d.Events[0].CheckID != "check_b" {
		t.Fatalf("events=%#v", d.Events)
	}
	if _, ok := d.Baselines["2222222"]; !ok {
		t.Fatal("subset commit erased unselected baseline")
	}
	incomplete := result(profile, "check_c", model.OutcomeIncomplete, "1111111")
	incomplete.Coverage[0].Continuity = model.ContinuityCoverage{State: "incomplete", Reason: "gap"}
	d, err = s.Commit(profile, incomplete, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.LastSuccess.CheckID != "check_b" || len(d.Events) != 1 || d.Baselines["1111111"].CheckID != "check_b" {
		t.Fatalf("incomplete advanced committed data: %#v", d)
	}
}

func TestPostRenameFailureReportsCommittedGeneration(t *testing.T) {
	profile := model.CheckProfile{ID: "family", Origin: "https://portal.example"}
	s := Store{Path: filepath.Join(t.TempDir(), "state.json")}
	r := result(profile, "check_a", model.OutcomeInitialBaseline, "1111111")
	r.Baseline.NewEnrolments = []string{"1111111"}
	commitTestHook = func(stage string) error {
		if stage == "after_rename" {
			return errors.New("directory sync failed")
		}
		return nil
	}
	defer func() { commitTestHook = nil }()
	_, err := s.Commit(profile, r, observations("1111111"))
	var ce *CommitError
	if !errors.As(err, &ce) || !ce.Committed {
		t.Fatalf("error=%#v", err)
	}
	d, loadErr := s.Load(profile)
	if loadErr != nil || d.LatestAttempt.CheckID != "check_a" {
		t.Fatalf("state=%#v err=%v", d, loadErr)
	}
}

func TestInvalidForeignAndLegacyStateArePreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	original := []byte("{}\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	profile := model.CheckProfile{ID: "family", Origin: "https://portal.example"}
	s := Store{Path: path}
	if _, err := s.Load(profile); err == nil {
		t.Fatal("malformed semantic state accepted")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, original) {
		t.Fatal("invalid state changed")
	}
	validPath := filepath.Join(dir, "valid.json")
	validStore := Store{Path: validPath}
	seed := result(profile, "check_seed", model.OutcomeInitialBaseline, "1111111")
	seed.Baseline.NewEnrolments = []string{"1111111"}
	if _, err := validStore.Commit(profile, seed, observations("1111111")); err != nil {
		t.Fatal(err)
	}
	if _, err := validStore.Load(model.CheckProfile{ID: "other", Origin: profile.Origin}); err == nil {
		t.Fatal("foreign account state accepted")
	}
	var raw map[string]any
	bytesValue, err := os.ReadFile(validPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bytesValue, &raw); err != nil {
		t.Fatal(err)
	}
	raw["schema_version"] = 2
	bytesValue, _ = json.Marshal(raw)
	if err := os.WriteFile(validPath, bytesValue, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validStore.Load(profile); err == nil {
		t.Fatal("unsupported version accepted")
	}
	for _, name := range []string{"changes.json", "latest.json", "checks.jsonl", "consumers/pas/changes.json", "coordination/account/schema-v2/latest.json", "coordination/account/check-status.json"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			legacy := filepath.Join(root, name)
			if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			s := Store{Path: filepath.Join(root, "new.json"), LegacyPaths: []string{legacy}}
			if _, err := s.Load(profile); err == nil {
				t.Fatal("legacy state accepted")
			}
			if _, err := os.Stat(legacy); err != nil {
				t.Fatal("legacy state was not preserved")
			}
		})
	}
}

func TestLoadRejectsSemanticallyCorruptCommittedResults(t *testing.T) {
	profile := model.CheckProfile{ID: "family", Origin: "https://portal.example"}
	for _, tc := range []struct {
		name    string
		corrupt func(map[string]any)
	}{
		{name: "missing coverage", corrupt: func(raw map[string]any) { raw["latest_attempt"].(map[string]any)["coverage"] = []any{} }},
		{name: "wrong count", corrupt: func(raw map[string]any) {
			raw["latest_attempt"].(map[string]any)["changes"].(map[string]any)["count"] = 1.0
		}},
		{name: "foreign new enrolment", corrupt: func(raw map[string]any) {
			raw["latest_attempt"].(map[string]any)["baseline"].(map[string]any)["new_enrolments"] = []any{"9999999"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Store{Path: filepath.Join(t.TempDir(), "state.json")}
			r := result(profile, "check_a", model.OutcomeInitialBaseline, "1111111")
			r.Baseline.NewEnrolments = []string{"1111111"}
			if _, err := s.Commit(profile, r, observations("1111111")); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(s.Path)
			var raw map[string]any
			if err := json.Unmarshal(b, &raw); err != nil {
				t.Fatal(err)
			}
			tc.corrupt(raw)
			b, _ = json.Marshal(raw)
			if err := os.WriteFile(s.Path, b, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Load(profile); err == nil {
				t.Fatal("corrupt result accepted")
			}
		})
	}
}

func TestKilledProcessBeforeRenameLeavesPriorGeneration(t *testing.T) {
	if os.Getenv("EDNEVNIK_STATE_CRASH_HELPER") == "1" {
		crashCommitHelper(t)
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	ready := filepath.Join(dir, "ready")
	profile := model.CheckProfile{ID: "family", Origin: "https://portal.example"}
	s := Store{Path: path}
	first := result(profile, "check_a", model.OutcomeInitialBaseline, "1111111")
	first.Baseline.NewEnrolments = []string{"1111111"}
	if _, err := s.Commit(profile, first, observations("1111111")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestKilledProcessBeforeRenameLeavesPriorGeneration")
	cmd.Env = append(os.Environ(), "EDNEVNIK_STATE_CRASH_HELPER=1", "EDNEVNIK_STATE_CRASH_PATH="+path, "EDNEVNIK_STATE_CRASH_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("helper did not reach pre-rename stage")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("killed process exposed a partial or new generation")
	}
	d, err := s.Load(profile)
	if err != nil || d.LastSuccess.CheckID != "check_a" {
		t.Fatalf("state=%#v err=%v", d, err)
	}
}

func crashCommitHelper(t *testing.T) {
	profile := model.CheckProfile{ID: "family", Origin: "https://portal.example"}
	s := Store{Path: os.Getenv("EDNEVNIK_STATE_CRASH_PATH")}
	ready := os.Getenv("EDNEVNIK_STATE_CRASH_READY")
	r := result(profile, "check_b", model.OutcomeCompleteWithChanges, "1111111")
	r.Changes = model.ChangeSummary{Count: 1, Items: []model.Change{{Kind: "grade_update", StudentID: "1111111", RecordID: "g1", Summary: "changed"}}}
	commitTestHook = func(stage string) error {
		if stage == "before_rename" {
			if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
				return err
			}
			select {}
		}
		return nil
	}
	_, _ = s.Commit(profile, r, observations("1111111"))
	t.Fatal("helper commit unexpectedly returned")
}
