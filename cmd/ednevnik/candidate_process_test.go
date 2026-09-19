package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kryzhovnik/ednevnik-cli/internal/checkstate"
	"github.com/kryzhovnik/ednevnik-cli/internal/model"
)

// TestCandidateFiveEnrolmentWorkload measures the real CLI transport path.
// The explicit check contract does not discover enrolments: initial baseline is
// 3 requests per enrolment, while a known one-page timeline adds one head
// revalidation per enrolment.
func TestCandidateFiveEnrolmentWorkload(t *testing.T) {
	binary := buildCandidateBinary(t)
	students := []string{"1000001", "1000002", "1000003", "1000004", "1000005"}
	var stage atomic.Int32
	var requests atomic.Int32
	var mu sync.Mutex
	byStage := map[int32]int{}
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		mu.Lock()
		byStage[stage.Load()]++
		mu.Unlock()
		student := r.URL.Query().Get("student")
		switch r.URL.Path {
		case "/grades":
			_, _ = fmt.Fprintf(w, `<a class="flex-table-row" href="/grades/7654321/show?student=%s"><div><strong class="d-block">Mathematics</strong></div></a>`, student)
		case "/absents":
			_, _ = io.WriteString(w, `<div class="categories-wrap"></div>`)
		case "/timeline-data":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			switch stage.Load() {
			case 0, 1:
				_, _ = io.WriteString(w, timelinePage(1, 1, timelineItem(100, "baseline")))
			case 2:
				if student == students[0] && page == 1 {
					_, _ = io.WriteString(w, timelinePage(1, 2, timelineItem(101, "new")))
				} else if student == students[0] {
					_, _ = io.WriteString(w, timelinePage(2, 2, timelineItem(100, "baseline")))
				} else {
					_, _ = io.WriteString(w, timelinePage(1, 1, timelineItem(100, "baseline")))
				}
			case 3:
				// No prior boundary appears within eight pages.
				_, _ = io.WriteString(w, timelinePage(page, 20, timelineItem(200+page, fmt.Sprintf("gap-%d", page))))
			default:
				http.Error(w, "unknown stage", http.StatusInternalServerError)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer portal.Close()
	root := t.TempDir()
	env := candidateEnv(root, portal.URL)
	checkArgs := []string{"check", "--profile=family"}
	for _, id := range students {
		checkArgs = append(checkArgs, "--student="+id)
	}
	runCheck := func(wantCode int) model.CheckResult {
		t.Helper()
		out, stderr, code := runCandidateCommand(t, binary, env, checkArgs...)
		if code != wantCode {
			t.Fatalf("check exit=%d want=%d stdout=%s stderr=%s", code, wantCode, out, stderr)
		}
		var result model.CheckResult
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatalf("decode check: %v: %s", err, out)
		}
		if len(result.Coverage) != len(students) {
			t.Fatalf("coverage=%d want=%d", len(result.Coverage), len(students))
		}
		return result
	}
	initial := runCheck(0)
	mu.Lock()
	initialRequests := byStage[0]
	mu.Unlock()
	if initial.Outcome != model.OutcomeInitialBaseline || initialRequests != 15 {
		t.Fatalf("initial outcome=%s requests=%d", initial.Outcome, initialRequests)
	}
	initialBaseline := initial.Baseline.ID
	stage.Store(1)
	unchanged := runCheck(0)
	mu.Lock()
	unchangedRequests := byStage[1]
	mu.Unlock()
	if unchanged.Outcome != model.OutcomeCompleteWithoutChanges || unchangedRequests != 20 {
		t.Fatalf("unchanged outcome=%s requests=%d", unchanged.Outcome, unchangedRequests)
	}
	stage.Store(2)
	changed := runCheck(0)
	mu.Lock()
	catchUpRequests := byStage[2]
	mu.Unlock()
	if changed.Outcome != model.OutcomeCompleteWithChanges || changed.Changes.Count != 1 || catchUpRequests != 21 {
		t.Fatalf("catch-up outcome=%s changes=%d requests=%d", changed.Outcome, changed.Changes.Count, catchUpRequests)
	}
	lastCompleteBaseline := changed.Baseline.ID
	stage.Store(3)
	incomplete := runCheck(3)
	mu.Lock()
	incompleteRequests := byStage[3]
	mu.Unlock()
	if incomplete.Outcome != model.OutcomeIncomplete || incompleteRequests != 50 || incomplete.Coverage[0].Continuity.Reason != "timeline_page_limit" {
		t.Fatalf("incomplete=%#v requests=%d", incomplete, incompleteRequests)
	}
	statusBytes, stderr, code := runCandidateCommand(t, binary, env, "status", "--profile=family")
	if code != 0 {
		t.Fatalf("status exit=%d stderr=%s", code, stderr)
	}
	var status model.CheckStatus
	if err := json.Unmarshal(statusBytes, &status); err != nil {
		t.Fatal(err)
	}
	if status.LastSuccess == nil || status.LastSuccess.Baseline.ID != lastCompleteBaseline || status.LastSuccess.Baseline.ID == initialBaseline {
		t.Fatalf("last complete baseline was not preserved: %#v", status.LastSuccess)
	}

	// A fresh measured baseline consumes 15. With a limit of 19 the next
	// five-enrolment check is refused on its fifth actual transport attempt.
	budgetRoot := t.TempDir()
	budgetEnv := append(candidateEnv(budgetRoot, portal.URL), "EDNEVNIK_DAILY_REQUEST_LIMIT=19")
	stage.Store(0)
	_, _, code = runCandidateCommand(t, binary, budgetEnv, checkArgs...)
	if code != 0 {
		t.Fatalf("budget baseline exit=%d", code)
	}
	stage.Store(1)
	_, refusal, code := runCandidateCommand(t, binary, budgetEnv, checkArgs...)
	if code != 1 || !bytes.Contains(refusal, []byte(`"reason":"refusal_quota"`)) {
		t.Fatalf("budget refusal exit=%d stderr=%s", code, refusal)
	}
	statusBytes, _, code = runCandidateCommand(t, binary, budgetEnv, "status", "--profile=family")
	if code != 0 || json.Unmarshal(statusBytes, &status) != nil || status.LastSuccess == nil || status.LastSuccess.Outcome != model.OutcomeInitialBaseline {
		t.Fatalf("budget refusal replaced baseline: code=%d status=%s", code, statusBytes)
	}
	_ = requests.Load() // documents that the server counter covers every request.
}

func TestCandidateHeadlessAuthCheckConsumerReplay(t *testing.T) {
	binary := buildCandidateBinary(t)
	var stage atomic.Int32
	var requests atomic.Int32
	var expectedSession atomic.Int32
	var submissions atomic.Int32
	expectedSession.Store(1)
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/login" {
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `<form action="/login"><input name="_token" value="csrf"><input name="password"></form>`)
				return
			}
			if err := r.ParseForm(); err != nil || r.Form.Get("username") != "parent" || r.Form.Get("password") != "synthetic-password" {
				http.Error(w, "rejected", http.StatusUnauthorized)
				return
			}
			submissions.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "session", Value: fmt.Sprintf("ok%d", expectedSession.Load()), Path: "/", HttpOnly: true})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if cookie, err := r.Cookie("session"); err != nil || cookie.Value != fmt.Sprintf("ok%d", expectedSession.Load()) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, `<div class="students-list"></div>`)
		case "/grades":
			_, _ = io.WriteString(w, `<div class="flex-table"></div>`)
		case "/absents":
			_, _ = io.WriteString(w, `<div class="categories-wrap"></div>`)
		case "/timeline-data":
			items := []string{timelineItem(100, "baseline")}
			for id := int32(1); id <= stage.Load(); id++ {
				items = append([]string{timelineItem(100+int(id), fmt.Sprintf("change-%d", id))}, items...)
			}
			_, _ = io.WriteString(w, timelinePage(1, 1, items...))
		default:
			http.NotFound(w, r)
		}
	}))
	defer portal.Close()
	root := t.TempDir()
	credentialPath := filepath.Join(t.TempDir(), "credentials.json")
	credential := fmt.Sprintf(`{"schema_version":1,"account":"family","origin":%q,"username":"parent","password":"synthetic-password"}`, canonicalOrigin(portal.URL))
	if err := os.WriteFile(credentialPath, []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(candidateEnv(root, portal.URL), "EDNEVNIK_CREDENTIAL_PROVIDER=file", "EDNEVNIK_TEST_CREDENTIALS_FILE="+credentialPath)
	must := func(args ...string) []byte {
		t.Helper()
		out, stderr, code := runCandidateCommand(t, binary, env, args...)
		if code != 0 {
			t.Fatalf("%v exit=%d stdout=%s stderr=%s", args, code, out, stderr)
		}
		return out
	}
	// Automatic file-provider recovery uses GET login, POST login, redirect GET,
	// and a separate protected verification: four requests before normal reads.
	must("check", "--profile=family", "--student=1234567")
	if got := requests.Load(); got != 7 {
		t.Fatalf("authenticated initial check requests=%d want=7", got)
	}
	must("consumer-register", "--profile=family", "--consumer=notify", "--start=earliest")
	must("consumer-register", "--profile=family", "--consumer=audit", "--start=earliest")
	expectedSession.Store(2) // expire the persisted cookie before the next protected read
	stage.Store(1)
	must("check", "--profile=family", "--student=1234567")
	if submissions.Load() != 2 || requests.Load() != 17 {
		t.Fatalf("expired-session recovery submissions=%d total requests=%d", submissions.Load(), requests.Load())
	}
	var first checkstate.Batch
	if err := json.Unmarshal(must("consumer-read", "--profile=family", "--consumer=notify", "--limit=1"), &first); err != nil || len(first.Events) != 1 {
		t.Fatalf("first batch=%#v err=%v", first, err)
	}
	// Simulate notification failure by leaving the delivered token unacknowledged.
	stage.Store(2)
	must("check", "--profile=family", "--student=1234567")
	var replay checkstate.Batch
	if err := json.Unmarshal(must("consumer-read", "--profile=family", "--consumer=notify"), &replay); err != nil || replay.Token != first.Token || replay.Events[0].ID != first.Events[0].ID {
		t.Fatalf("replay=%#v first=%#v err=%v", replay, first, err)
	}
	beforeLocal := requests.Load()
	must("consumer-ack", "--profile=family", "--consumer=notify", "--token="+first.Token)
	var remaining, independent checkstate.Batch
	_ = json.Unmarshal(must("consumer-read", "--profile=family", "--consumer=notify"), &remaining)
	_ = json.Unmarshal(must("consumer-read", "--profile=family", "--consumer=audit"), &independent)
	if requests.Load() != beforeLocal || len(remaining.Events) != 1 || len(independent.Events) != 2 {
		t.Fatalf("remaining=%#v independent=%#v localHTTP=%d", remaining, independent, requests.Load()-beforeLocal)
	}
}

func TestCandidateDefaultRequestPacing(t *testing.T) {
	binary := buildCandidateBinary(t)
	var mu sync.Mutex
	requestTimes := []time.Time{}
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestTimes = append(requestTimes, time.Now())
		mu.Unlock()
		switch r.URL.Path {
		case "/grades":
			_, _ = io.WriteString(w, `<div class="flex-table"></div>`)
		case "/absents":
			_, _ = io.WriteString(w, `<div class="categories-wrap"></div>`)
		case "/timeline-data":
			_, _ = io.WriteString(w, timelinePage(1, 1))
		default:
			http.NotFound(w, r)
		}
	}))
	defer portal.Close()
	env := candidateEnv(t.TempDir(), portal.URL)
	for i := 0; i < len(env); i++ {
		if strings.HasPrefix(env[i], "EDNEVNIK_REQUEST_INTERVAL=") {
			env = append(env[:i], env[i+1:]...)
			break
		}
	}
	out, stderr, code := runCandidateCommand(t, binary, env, "check", "--profile=family", "--student=1234567")
	if code != 0 {
		t.Fatalf("default-paced check exit=%d stdout=%s stderr=%s", code, out, stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requestTimes) != 3 {
		t.Fatalf("requests=%d want=3", len(requestTimes))
	}
	if elapsed := requestTimes[2].Sub(requestTimes[0]); elapsed < 4900*time.Millisecond {
		t.Fatalf("default request pacing too short: %s", elapsed)
	}
}

func buildCandidateBinary(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "ednevnik")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	return binary
}

func candidateEnv(root, portalURL string) []string {
	home := filepath.Join(root, "home")
	tmp := filepath.Join(root, "tmp")
	_ = os.MkdirAll(home, 0o700)
	_ = os.MkdirAll(tmp, 0o700)
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + tmp, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "EDNEVNIK_TEST_ALLOW_HTTP_LOOPBACK=1", "EDNEVNIK_TEST_STATE_ROOT=" + root, "EDNEVNIK_TEST_STATE_DIR=" + root, "EDNEVNIK_BASE_URL=" + portalURL, "EDNEVNIK_REQUEST_INTERVAL=0s", "EDNEVNIK_MIN_CHECK_INTERVAL=0s", "EDNEVNIK_COMMAND_TIMEOUT=10s", "EDNEVNIK_USERNAME=", "EDNEVNIK_PASSWORD=", "EDNEVNIK_CREDENTIALS_FILE="}
}

func runCandidateCommand(t *testing.T, binary string, env []string, args ...string) (stdout, stderr []byte, exitCode int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(nil)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if err == nil {
		return out.Bytes(), errOut.Bytes(), 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return out.Bytes(), errOut.Bytes(), exit.ExitCode()
	}
	t.Fatalf("run %v: %v", args, err)
	return nil, nil, -1
}
