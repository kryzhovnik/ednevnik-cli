package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kryzhovnik/ednevnik/internal/client"
	"github.com/kryzhovnik/ednevnik/internal/model"
	"github.com/kryzhovnik/ednevnik/internal/parse"
	"github.com/kryzhovnik/ednevnik/internal/store"
)

type fakeClient struct {
	second  bool
	invalid bool
}

func (f *fakeClient) Login(context.Context, string, string) error { return nil }
func (f *fakeClient) BudgetStatus() (string, int, int, error)     { return "2026-09-12", 0, 100, nil }
func (f *fakeClient) Get(_ context.Context, path string) ([]byte, error) {
	if path == "/grades?student=1234567" {
		if f.invalid {
			return []byte(`<html><title>Maintenance</title></html>`), nil
		}
		grades := `<div class="grade numeric">4</div>`
		if f.second {
			grades += `<div class="grade numeric">5</div>`
		}
		return []byte(`<a class="flex-table-row" href="/grades/7654321/show?student=1234567"><div><strong class="d-block">Mathematics</strong><em>Teacher</em></div><div class="grades-cell-wrap">` + grades + `</div></a>`), nil
	}
	if path == "/absents?student=1234567" {
		return []byte(`<div class="categories-wrap"><div class="category-wrap"><div class="category-top">08. 09. 2026.</div><div class="category-item-wrap green"><span class="category-symbol-subtitle">2. час</span><div class="name">Mathematics</div></div></div></div>`), nil
	}
	if path == "/timeline-data?page=1&student=1234567" {
		return []byte(`{"success":true,"meta":{"currentPage":1,"nextPage":null,"lastPage":1},"data":[]}`), nil
	}
	return nil, os.ErrNotExist
}

type scriptedCheckRunner struct {
	result checkResult
	err    error
}

func (r scriptedCheckRunner) Check(context.Context, checkRequest) (checkResult, error) {
	return r.result, r.err
}

func captureStdout(t *testing.T, fn func() error) ([]byte, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, err = buf.ReadFrom(r)
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), runErr
}

func TestCheckContractScriptedOutcomes(t *testing.T) {
	tests := []struct {
		name, outcome string
		count         int
	}{
		{name: "initial", outcome: "initial_baseline"},
		{name: "unchanged", outcome: "complete_without_changes"},
		{name: "changed", outcome: "complete_with_changes", count: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			result := checkResult{
				Outcome:  tt.outcome,
				Coverage: []enrolmentCoverage{{EnrolmentID: "1234567", Sections: []sectionCoverage{{Name: "grades", State: "complete", Representation: "overview"}, {Name: "absences", State: "complete", Representation: "current_records"}, {Name: "timeline", State: "complete", Representation: "caught_up_pages"}}, Continuity: continuityCoverage{State: "complete"}}},
				Baseline: baselineReference{ID: "baseline-1"},
				Changes:  changeSummary{Count: tt.count, Items: []model.Change{}},
				Guidance: guidance{Action: "Read retained events with the consumer batch command when available."},
			}
			a := &app{dir: dir, origin: "https://portal.example", checker: scriptedCheckRunner{result: result}}
			data, err := captureStdout(t, func() error {
				return a.check(context.Background(), []string{"--profile", "family", "--student", "1234567", "--student", "1234567"})
			})
			if err != nil {
				t.Fatal(err)
			}
			var got checkResult
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.SchemaVersion != 3 || got.Outcome != tt.outcome || len(got.Requested) != 1 || got.Profile.ID != "family" || got.Profile.Origin != "https://portal.example" {
				t.Fatalf("result=%#v", got)
			}
			var status checkStatus
			if err := store.LoadSnapshot(filepath.Join(dir, "profiles", "family", "check-status.json"), &status); err != nil {
				t.Fatal(err)
			}
			if status.LastSuccess == nil || status.LatestAttempt == nil || status.History != "latest_attempt_only" {
				t.Fatalf("status=%#v", status)
			}
		})
	}
}

func TestCheckContractInjectedErrorRecordsFailedAttempt(t *testing.T) {
	dir := t.TempDir()
	a := &app{dir: dir, origin: "https://portal.example", checker: scriptedCheckRunner{err: &checkFailure{Reason: "refusal_quota", Err: errors.New("daily request budget exhausted"), Retryable: true, Action: "Retry after the local reset time."}}}
	_, err := captureStdout(t, func() error {
		return a.check(context.Background(), []string{"--profile", "family", "--student", "1234567"})
	})
	var got *commandError
	if !errors.As(err, &got) || got.body.Reason != "refusal_quota" {
		t.Fatalf("err=%#v", err)
	}
	if got.body.Profile == nil || got.body.Profile.ID != "family" || len(got.body.Requested) != 1 {
		t.Fatalf("error contract=%#v", got.body)
	}
	var status checkStatus
	if err := store.LoadSnapshot(filepath.Join(dir, "profiles", "family", "check-status.json"), &status); err != nil {
		t.Fatal(err)
	}
	if status.LatestAttempt == nil || status.LatestAttempt.Outcome != "failed" || status.LatestAttempt.Failure == nil || status.LatestAttempt.Failure.Reason != "refusal_quota" || status.LastSuccess != nil {
		t.Fatalf("status=%#v", status)
	}
}

func TestCheckRejectsInconsistentIncompleteResult(t *testing.T) {
	dir := t.TempDir()
	a := &app{dir: dir, origin: "https://portal.example", checker: scriptedCheckRunner{result: checkResult{
		Outcome:  model.OutcomeIncomplete,
		Baseline: baselineReference{ID: "must-not-commit"},
		Coverage: []enrolmentCoverage{{EnrolmentID: "1234567", Sections: []sectionCoverage{}, Continuity: continuityCoverage{State: "incomplete"}}},
	}}}
	_, err := captureStdout(t, func() error {
		return a.check(context.Background(), []string{"--profile", "family", "--student", "1234567"})
	})
	var got *commandError
	if !errors.As(err, &got) || got.body.Reason != model.ReasonInvalidState {
		t.Fatalf("err=%#v", err)
	}
}

func TestLiveCheckUsesCurrentParsersAndReportsIncompleteContinuity(t *testing.T) {
	dir := t.TempDir()
	a := &app{dir: dir, origin: "https://portal.example", client: &fakeClient{}}
	data, err := captureStdout(t, func() error {
		return a.check(context.Background(), []string{"--profile", "family", "--student", "1234567"})
	})
	var statusErr *exitStatus
	if !errors.As(err, &statusErr) || statusErr.code != 3 {
		t.Fatalf("err=%#v", err)
	}
	var got checkResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Outcome != "incomplete" || got.Coverage[0].Continuity.Reason != "timeline_catch_up_not_implemented" || got.Baseline.ID != "" {
		t.Fatalf("result=%#v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "latest.json")); !os.IsNotExist(err) {
		t.Fatalf("live check committed legacy baseline: %v", err)
	}
}

func TestStatusIsLocalAndSeparatesLatestAttemptFromLastSuccess(t *testing.T) {
	dir := t.TempDir()
	success := checkResult{SchemaVersion: 3, CheckID: "ok", Outcome: "complete_without_changes", Profile: checkProfile{ID: "family", Origin: "https://portal.example"}, CompletedAt: time.Now(), Baseline: baselineReference{ID: "baseline-1", NewEnrolments: []string{}}, Changes: changeSummary{Items: []model.Change{}}}
	failed := checkResult{SchemaVersion: 3, CheckID: "failed", Outcome: "failed", Profile: checkProfile{ID: "family", Origin: "https://portal.example"}, CompletedAt: time.Now(), Baseline: baselineReference{NewEnrolments: []string{}}, Changes: changeSummary{Items: []model.Change{}}, Failure: &model.FailureSummary{Reason: "io"}}
	a := &app{dir: dir, origin: "https://portal.example"}
	if err := a.recordCheckStatus(success); err != nil {
		t.Fatal(err)
	}
	if err := a.recordCheckStatus(failed); err != nil {
		t.Fatal(err)
	}
	data, err := captureStdout(t, func() error { return a.status([]string{"--profile", "family"}) })
	if err != nil {
		t.Fatal(err)
	}
	var got checkStatus
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.LatestAttempt.CheckID != "failed" || got.LastSuccess.CheckID != "ok" {
		t.Fatalf("status=%#v", got)
	}
}

func TestRecordCheckStatusPreservesUnsupportedExistingState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles", "family", "check-status.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("{}\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{dir: dir, origin: "https://portal.example"}
	result := checkResult{SchemaVersion: 3, CheckID: "new", Outcome: model.OutcomeIncomplete, Profile: checkProfile{ID: "family", Origin: a.origin}, Coverage: []enrolmentCoverage{}, Baseline: baselineReference{NewEnrolments: []string{}}, Changes: changeSummary{Items: []model.Change{}}}
	var invalid *invalidCheckStatusError
	if err := a.recordCheckStatus(result); !errors.As(err, &invalid) {
		t.Fatalf("err=%v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("state changed: %q", got)
	}
}

func TestProcessErrorsAreJSONOnly(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "ednevnik")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	for _, args := range [][]string{nil, {"unknown"}, {"check", "--bad-flag"}} {
		cmd := exec.Command(binary, args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
			t.Fatalf("args=%v err=%v", args, err)
		}
		if stdout.Len() != 0 || strings.Count(strings.TrimSpace(stderr.String()), "\n") != 0 {
			t.Fatalf("args=%v stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
		var got contractError
		if err := json.Unmarshal(stderr.Bytes(), &got); err != nil || got.Reason != "invalid_argument" {
			t.Fatalf("args=%v stderr=%q err=%v", args, stderr.String(), err)
		}
	}
	cmd := exec.Command(binary, "students")
	cmd.Env = append(os.Environ(), "EDNEVNIK_BASE_URL=https://user:do-not-echo@example.test/path?secret=yes")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 || strings.Contains(stderr.String(), "do-not-echo") || strings.Contains(stderr.String(), "secret=yes") {
		t.Fatalf("unsafe origin error: %q", stderr.String())
	}
	var got contractError
	if err := json.Unmarshal(stderr.Bytes(), &got); err != nil || got.Reason != model.ReasonInvalidArgument {
		t.Fatalf("stderr=%q err=%v", stderr.String(), err)
	}
}

func TestProcessLiveIncompleteAndLocalRecoveryCommands(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "ednevnik")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}

	var portalMode atomic.Value
	portalMode.Store("valid")
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/grades":
			if portalMode.Load() == "maintenance" {
				_, _ = w.Write([]byte(`<html><title>Maintenance</title></html>`))
				return
			}
			if portalMode.Load() == "oversized" {
				_, _ = w.Write(bytes.Repeat([]byte("x"), (20<<20)+1))
				return
			}
			_, _ = w.Write([]byte(`<a class="flex-table-row" href="/grades/7654321/show?student=1234567"><div><strong class="d-block">Mathematics</strong></div></a>`))
		case "/absents":
			_, _ = w.Write([]byte(`<div class="categories-wrap"></div>`))
		case "/timeline-data":
			if portalMode.Load() == "success-only" {
				_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"meta":{"currentPage":1,"nextPage":null,"lastPage":1},"data":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer portal.Close()
	configDir, stateDir := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(configDir, "ednevnik"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "ednevnik", "session.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binary, "check", "--profile", "family", "--student", "1234567")
	cmd.Env = append(os.Environ(), "EDNEVNIK_TEST_ALLOW_HTTP_LOOPBACK=1", "EDNEVNIK_TEST_STATE_ROOT="+t.TempDir(), "EDNEVNIK_STATE_DIR="+stateDir, "EDNEVNIK_BASE_URL="+portal.URL)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 || stderr.Len() != 0 {
		t.Fatalf("err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	var incomplete checkResult
	if err := json.Unmarshal(stdout.Bytes(), &incomplete); err != nil || incomplete.Outcome != model.OutcomeIncomplete {
		t.Fatalf("stdout=%q err=%v", stdout.String(), err)
	}

	// Seed an earlier complete result and prove that source failures update only
	// the latest attempt. The successful baseline remains available to status.
	success := incomplete
	success.CheckID = "known-good"
	success.Outcome = model.OutcomeCompleteWithoutChanges
	success.Coverage[0].Continuity = continuityCoverage{State: "complete"}
	success.Baseline = baselineReference{ID: "baseline-known-good", NewEnrolments: []string{}}
	status := checkStatus{SchemaVersion: checkSchemaVersion, History: "latest_attempt_only", LatestAttempt: &success, LastSuccess: &success}
	statusPath := filepath.Join(stateDir, "profiles", "family", "check-status.json")
	if err := store.SaveSnapshot(statusPath, status); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"maintenance", "success-only", "oversized"} {
		portalMode.Store(mode)
		cmd = exec.Command(binary, "check", "--profile", "family", "--student", "1234567")
		cmd.Env = append(os.Environ(), "EDNEVNIK_TEST_ALLOW_HTTP_LOOPBACK=1", "EDNEVNIK_TEST_STATE_ROOT="+t.TempDir(), "EDNEVNIK_STATE_DIR="+stateDir, "EDNEVNIK_BASE_URL="+portal.URL)
		stdout.Reset()
		stderr.Reset()
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err = cmd.Run()
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || stdout.Len() != 0 {
			t.Fatalf("mode=%s err=%v stdout=%q stderr=%q", mode, err, stdout.String(), stderr.String())
		}
		var sourceErr contractError
		if err := json.Unmarshal(stderr.Bytes(), &sourceErr); err != nil || sourceErr.Reason != model.ReasonInvalidSource || strings.Contains(stderr.String(), strings.Repeat("x", 32)) {
			t.Fatalf("mode=%s stderr=%q err=%v", mode, stderr.String(), err)
		}
		var preserved checkStatus
		if err := store.LoadSnapshot(statusPath, &preserved); err != nil || preserved.LastSuccess == nil || preserved.LastSuccess.Baseline.ID != "baseline-known-good" {
			t.Fatalf("mode=%s preserved=%#v err=%v", mode, preserved, err)
		}
	}
	portalMode.Store("maintenance")
	cmd = exec.Command(binary, "subjects", "--student", "1234567")
	cmd.Env = append(os.Environ(), "EDNEVNIK_TEST_ALLOW_HTTP_LOOPBACK=1", "EDNEVNIK_TEST_STATE_ROOT="+t.TempDir(), "EDNEVNIK_STATE_DIR="+stateDir, "EDNEVNIK_BASE_URL="+portal.URL)
	stdout.Reset()
	stderr.Reset()
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || stdout.Len() != 0 {
		t.Fatalf("focused command err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	var focusedErr contractError
	if err := json.Unmarshal(stderr.Bytes(), &focusedErr); err != nil || focusedErr.Reason != model.ReasonInvalidSource {
		t.Fatalf("focused stderr=%q err=%v", stderr.String(), err)
	}
	portalMode.Store("valid")

	invalidPath := statusPath
	if err := os.WriteFile(invalidPath, []byte(`{"schema_version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(binary, "status", "--profile", "family")
	cmd.Env = append(os.Environ(), "EDNEVNIK_STATE_DIR="+stateDir, "EDNEVNIK_BASE_URL="+portal.URL)
	stdout.Reset()
	stderr.Reset()
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || stdout.Len() != 0 {
		t.Fatalf("err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	var contractErr contractError
	if err := json.Unmarshal(stderr.Bytes(), &contractErr); err != nil || contractErr.Reason != model.ReasonInvalidState {
		t.Fatalf("stderr=%q err=%v", stderr.String(), err)
	}

	for _, args := range [][]string{{"help"}, {"version"}, {"status"}, {"changes"}} {
		cmd = exec.Command(binary, args...)
		cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+configDir, "EDNEVNIK_STATE_DIR="+t.TempDir(), "EDNEVNIK_BASE_URL=://invalid")
		err = cmd.Run()
		if args[0] != "changes" && err != nil {
			t.Fatalf("local command %v read invalid configuration: %v", args, err)
		}
		if args[0] == "changes" && err == nil {
			t.Fatal("changes unexpectedly found state")
		}
	}
}

type retryClient struct {
	gets   int
	logins int
}

func (f *retryClient) Login(context.Context, string, string) error {
	f.logins++
	return nil
}
func (f *retryClient) BudgetStatus() (string, int, int, error) { return "2026-09-12", 0, 100, nil }
func (f *retryClient) Get(context.Context, string) ([]byte, error) {
	f.gets++
	if f.gets == 1 {
		return nil, client.ErrNotAuthenticated
	}
	return []byte("ok"), nil
}

type fakeCredentials struct{}

func (fakeCredentials) PromptSave(string) error       { return nil }
func (fakeCredentials) Load() (string, string, error) { return "user", "password", nil }

func TestGetAutomaticallyLogsInAndRetries(t *testing.T) {
	fake := &retryClient{}
	a := &app{client: fake, creds: fakeCredentials{}}
	body, err := a.get(context.Background(), "/grades")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ok" || fake.gets != 2 || fake.logins != 1 {
		t.Fatalf("body=%q gets=%d logins=%d", body, fake.gets, fake.logins)
	}
}

func TestSyncWritesSnapshotAndChanges(t *testing.T) {
	fake := &fakeClient{}
	a := &app{client: fake, dir: t.TempDir()}
	if err := a.sync(context.Background(), []string{"--student", "1234567"}); err != nil {
		t.Fatal(err)
	}
	var first model.Snapshot
	if err := store.LoadSnapshot(filepath.Join(a.dir, "latest.json"), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Students) != 1 || len(first.Students[0].Subjects) != 1 || len(first.Students[0].Absences) != 1 {
		t.Fatalf("snapshot=%#v", first)
	}
	fake.second = true
	if err := a.sync(context.Background(), []string{"--force", "--student", "1234567"}); err != nil {
		t.Fatal(err)
	}
	var changes model.Changes
	if err := store.LoadSnapshot(filepath.Join(a.dir, "changes.json"), &changes); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, change := range changes.Items {
		if change.Kind == "subject_grades_changed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("changes=%#v", changes)
	}
	latest := filepath.Join(a.dir, "latest.json")
	before, err := os.ReadFile(latest)
	if err != nil {
		t.Fatal(err)
	}
	fake.invalid = true
	if err := a.sync(context.Background(), []string{"--force", "--student", "1234567"}); !errors.Is(err, parse.ErrInvalidSource) {
		t.Fatalf("invalid sync err=%v", err)
	}
	after, err := os.ReadFile(latest)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("valid baseline changed after invalid source: err=%v", err)
	}
}

func TestConsumerUsesIndependentStateDirectory(t *testing.T) {
	a := &app{dir: t.TempDir()}
	defaultDir, err := a.consumerDir("")
	if err != nil || defaultDir != a.dir {
		t.Fatalf("defaultDir=%q err=%v", defaultDir, err)
	}
	pasDir, err := a.consumerDir("pas")
	if err != nil || pasDir != filepath.Join(a.dir, "consumers", "pas") {
		t.Fatalf("pasDir=%q err=%v", pasDir, err)
	}
	if _, err := a.consumerDir("../pas"); err == nil {
		t.Fatal("unsafe consumer name accepted")
	}
}

type timelineClient struct{ paths []string }

func (f *timelineClient) Login(context.Context, string, string) error { return nil }
func (f *timelineClient) BudgetStatus() (string, int, int, error)     { return "2026-09-12", 0, 100, nil }
func (f *timelineClient) Get(_ context.Context, path string) ([]byte, error) {
	f.paths = append(f.paths, path)
	if path == "/timeline-data?page=1&student=1234567" {
		return []byte(`{"success":true,"meta":{"currentPage":1,"nextPage":2,"lastPage":2},"data":[{"date":{"day":"Monday"},"items":[{"id":1,"title":"Math","itemType":"activity"}]}]}`), nil
	}
	if path == "/timeline-data?page=2&student=1234567" {
		return []byte(`{"success":true,"meta":{"currentPage":2,"nextPage":null,"lastPage":2},"data":[{"date":{"day":"Sunday"},"items":[{"id":2,"title":"English","itemType":"activity"}]}]}`), nil
	}
	return nil, os.ErrNotExist
}

type discoveryClient struct{ home []byte }

func (f *discoveryClient) Login(context.Context, string, string) error { return nil }
func (f *discoveryClient) BudgetStatus() (string, int, int, error)     { return "2026-09-12", 0, 100, nil }
func (f *discoveryClient) Get(_ context.Context, path string) ([]byte, error) {
	if path == "/" {
		return f.home, nil
	}
	return nil, fmt.Errorf("unexpected fetch %s", path)
}

func TestSyncCurrentRejectsMissingExpectedAndNoCurrentEnrolments(t *testing.T) {
	dir := t.TempDir()
	previous := model.Snapshot{SchemaVersion: model.SchemaVersion, FetchedAt: time.Now().Add(-time.Hour), Students: []model.StudentData{
		{Student: model.Student{ID: "1111111", Current: true}},
		{Student: model.Student{ID: "2222222", Current: true}},
	}}
	latest := filepath.Join(dir, "latest.json")
	if err := store.SaveSnapshot(latest, previous); err != nil {
		t.Fatal(err)
	}
	home := []byte(`<div class="card student"><div class="card-header"><h5>Child</h5></div><a class="student-school-class-wrap" href="/?student=1111111"><div class="student-school-class-item">School</div><div class="student-school-class-item school-class-strong">V a</div><div class="student-school-class-item">26/27</div></a></div>`)
	a := &app{client: &discoveryClient{home: home}, dir: dir}
	if err := a.sync(context.Background(), []string{"--current", "--force"}); err == nil || !strings.Contains(err.Error(), "2222222") {
		t.Fatalf("missing expected enrolment err=%v", err)
	}
	var preserved model.Snapshot
	if err := store.LoadSnapshot(latest, &preserved); err != nil || len(preserved.Students) != 2 {
		t.Fatalf("preserved=%#v err=%v", preserved, err)
	}

	withdrawn := []byte(`<div class="card student"><div class="card-header"><h5>Child</h5></div><a class="student-school-class-wrap" href="/?student=1111111"><div class="student-school-class-item">School</div><div class="student-school-class-item school-class-strong">Исписан</div><div class="student-school-class-item">26/27</div></a></div>`)
	a = &app{client: &discoveryClient{home: withdrawn}, dir: t.TempDir()}
	if err := a.sync(context.Background(), []string{"--current", "--force"}); err == nil || !strings.Contains(err.Error(), "no_current_enrolments") {
		t.Fatalf("no-current err=%v", err)
	}
}

func TestTimelineLoadsAllPages(t *testing.T) {
	fake := &timelineClient{}
	a := &app{client: fake, dir: t.TempDir()}
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = a.timeline(context.Background(), []string{"--student", "1234567", "--all"})
	w.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	var page model.ActivityPage
	if err := json.Unmarshal(buf.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || len(fake.paths) != 2 {
		t.Fatalf("page=%#v paths=%#v", page, fake.paths)
	}
}
