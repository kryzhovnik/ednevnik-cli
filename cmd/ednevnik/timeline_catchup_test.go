package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kryzhovnik/ednevnik-cli/internal/checkstate"
	"github.com/kryzhovnik/ednevnik-cli/internal/model"
	"github.com/kryzhovnik/ednevnik-cli/internal/parse"
)

type timelineScriptClient struct {
	mu       sync.Mutex
	pages    map[int]string
	requests []int
}

type shiftingHeadClient struct {
	*timelineScriptClient
	headReads int
}

type liveVariantClient struct {
	*timelineScriptClient
}

func (c *liveVariantClient) Get(ctx context.Context, path string) ([]byte, error) {
	u, _ := url.Parse(path)
	switch u.Path {
	case "/grades":
		return []byte(`<div class="flex-table"><a class="flex-table-row" href="/grades/7654321/show"><strong class="d-block">Mathematics</strong></a></div>`), nil
	case "/absents":
		return []byte(`<html><body><div><div class="main-content container"><div class="ee-container"><div class="stats-wrap mb-3"></div><no-data></no-data><absent-modal name="absence" submit-url="/absences"></absent-modal></div></div></div></body></html>`), nil
	default:
		return c.timelineScriptClient.Get(ctx, path)
	}
}

func (c *shiftingHeadClient) Get(ctx context.Context, path string) ([]byte, error) {
	u, _ := url.Parse(path)
	if u.Path == "/timeline-data" && u.Query().Get("page") == "1" {
		c.headReads++
		if c.headReads == 2 {
			return []byte(timelinePage(1, 2, timelineItem(301, "inserted"), timelineItem(200, "new"))), nil
		}
	}
	return c.timelineScriptClient.Get(ctx, path)
}

func (c *timelineScriptClient) Login(context.Context, string, string) error { return nil }
func (c *timelineScriptClient) BudgetStatus() (string, int, int, error)     { return "", 0, 0, nil }
func (c *timelineScriptClient) Get(_ context.Context, path string) ([]byte, error) {
	u, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	switch u.Path {
	case "/grades":
		return []byte(`<a class="flex-table-row" href="/grades/7654321/show?student=1234567"><div><strong class="d-block">Mathematics</strong></div></a>`), nil
	case "/absents":
		return []byte(`<div class="categories-wrap"></div>`), nil
	case "/timeline-data":
		page, err := strconv.Atoi(u.Query().Get("page"))
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.requests = append(c.requests, page)
		body, ok := c.pages[page]
		if !ok {
			return nil, fmt.Errorf("unexpected timeline page %d", page)
		}
		return []byte(body), nil
	default:
		return nil, fmt.Errorf("unexpected path %s", path)
	}
}

func timelinePage(page, last int, items ...string) string {
	next := "null"
	if page < last {
		next = strconv.Itoa(page + 1)
	}
	data := "[]"
	if len(items) > 0 {
		data = `[{"date":{"day":"Monday"},"items":[` + joinJSON(items) + `]}]`
	}
	return fmt.Sprintf(`{"success":true,"meta":{"currentPage":%d,"nextPage":%s,"lastPage":%d},"data":%s}`, page, next, last, data)
}

func timelineItem(id int, title string) string {
	return fmt.Sprintf(`{"id":%d,"title":%q,"itemType":"activity","note":%q}`, id, title, title+" note")
}

func joinJSON(items []string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += ","
		}
		out += item
	}
	return out
}

func runCheckResult(t *testing.T, a *app) (checkResult, error) {
	t.Helper()
	data, err := captureStdout(t, func() error {
		return a.check(context.Background(), []string{"--profile=family", "--student=1234567"})
	})
	var result checkResult
	if unmarshalErr := json.Unmarshal(data, &result); unmarshalErr != nil {
		t.Fatalf("decode check result %q: %v", data, unmarshalErr)
	}
	return result, err
}

func TestLiveCheckAcceptsVerifiedOverviewAndEmptyAbsenceVariants(t *testing.T) {
	client := &liveVariantClient{timelineScriptClient: &timelineScriptClient{pages: map[int]string{1: timelinePage(1, 1)}}}
	runner := liveCheckRunner{app: &app{client: client}}
	run, err := runner.Check(context.Background(), checkRequest{CheckID: "check_test", ProfileID: "family", Enrolments: []string{"1234567"}, Prior: map[string]checkstate.Baseline{}})
	if err != nil {
		t.Fatal(err)
	}
	if run.Result.Outcome != model.OutcomeInitialBaseline || len(run.Observations) != 1 {
		t.Fatalf("run = %#v", run)
	}
	sections := run.Result.Coverage[0].Sections
	if len(sections) != 3 || sections[0].State != "complete" || sections[1].State != "complete" {
		t.Fatalf("sections = %#v", sections)
	}
}

func TestTimelineCatchUpCommitsAllPagesThroughOverlap(t *testing.T) {
	dir := t.TempDir()
	client := &timelineScriptClient{pages: map[int]string{1: timelinePage(1, 1, timelineItem(100, "old"))}}
	a := &app{dir: dir, origin: "https://portal.example", client: client}
	initial, err := runCheckResult(t, a)
	if err != nil || initial.Outcome != model.OutcomeInitialBaseline {
		t.Fatalf("initial=%#v err=%v", initial, err)
	}

	client.pages = map[int]string{
		1: timelinePage(1, 2, timelineItem(103, "new-3"), timelineItem(102, "new-2")),
		2: timelinePage(2, 2, timelineItem(101, "new-1"), timelineItem(100, "old")),
	}
	result, err := runCheckResult(t, a)
	if err != nil || result.Outcome != model.OutcomeCompleteWithChanges || result.Changes.Count != 3 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if got := result.Coverage[0]; got.Continuity.Reason != "timeline_overlap_established" || got.Sections[2].Representation != "caught_up_pages" {
		t.Fatalf("coverage=%#v", got)
	} else if got.Sections[2].FirstPage != 1 || got.Sections[2].LastPage != 2 || got.Sections[2].PageCount != 2 || got.Sections[2].RecordsInspected != 4 || got.Sections[2].CorrectionCoverage != "inspected_timeline_pages_only" {
		t.Fatalf("timeline inspection evidence=%#v", got.Sections[2])
	}
	doc, err := a.checkStateStore("family").Load(checkProfile{ID: "family", Origin: a.origin})
	if err != nil {
		t.Fatal(err)
	}
	baseline := doc.Baselines["1234567"]
	if len(baseline.Snapshot.Activities) != 4 || len(doc.Events) != 3 || len(baseline.TimelineBoundary) != 2 {
		t.Fatalf("baseline=%#v events=%#v", baseline, doc.Events)
	}
}

func TestTimelineCatchUpDoesNotEmitUnknownSuffixAfterOverlap(t *testing.T) {
	dir := t.TempDir()
	client := &timelineScriptClient{pages: map[int]string{1: timelinePage(1, 1, timelineItem(100, "anchor-a"), timelineItem(99, "anchor-b"))}}
	a := &app{dir: dir, origin: "https://portal.example", client: client}
	if _, err := runCheckResult(t, a); err != nil {
		t.Fatal(err)
	}
	client.pages = map[int]string{
		1: timelinePage(1, 2, timelineItem(102, "new-x"), timelineItem(101, "new-y")),
		2: timelinePage(2, 2, timelineItem(100, "anchor-a"), timelineItem(99, "anchor-b"), timelineItem(50, "older-unseen")),
	}
	result, err := runCheckResult(t, a)
	if err != nil || result.Changes.Count != 2 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	for _, change := range result.Changes.Items {
		if change.Subject == "older-unseen" {
			t.Fatalf("historical suffix emitted as new: %#v", result.Changes.Items)
		}
	}
}

func TestTimelineCatchUpTraversesEmptyNonfinalPageForEmptyBoundary(t *testing.T) {
	dir := t.TempDir()
	client := &timelineScriptClient{pages: map[int]string{1: timelinePage(1, 1)}}
	a := &app{dir: dir, origin: "https://portal.example", client: client}
	if _, err := runCheckResult(t, a); err != nil {
		t.Fatal(err)
	}
	client.pages = map[int]string{
		1: timelinePage(1, 2),
		2: timelinePage(2, 2, timelineItem(100, "new-after-empty")),
	}
	result, err := runCheckResult(t, a)
	if err != nil || result.Outcome != model.OutcomeCompleteWithChanges || result.Changes.Count != 1 || len(client.requests) < 4 {
		t.Fatalf("result=%#v requests=%v err=%v", result, client.requests, err)
	}
}

func TestInitialEmptyNonfinalTimelineDoesNotFloodNextCheck(t *testing.T) {
	dir := t.TempDir()
	client := &timelineScriptClient{pages: map[int]string{
		1: timelinePage(1, 2),
		2: timelinePage(2, 2, timelineItem(100, "historical")),
	}}
	a := &app{dir: dir, origin: "https://portal.example", client: client}
	initial, err := runCheckResult(t, a)
	if err != nil || initial.Outcome != model.OutcomeInitialBaseline || initial.Changes.Count != 0 || initial.Coverage[0].Sections[2].Representation != "recent_baseline_pages" {
		t.Fatalf("initial=%#v err=%v", initial, err)
	}
	unchanged, err := runCheckResult(t, a)
	if err != nil || unchanged.Outcome != model.OutcomeCompleteWithoutChanges || unchanged.Changes.Count != 0 {
		t.Fatalf("unchanged=%#v err=%v", unchanged, err)
	}
	doc, err := a.checkStateStore("family").Load(checkProfile{ID: "family", Origin: a.origin})
	if err != nil || len(doc.Events) != 0 || len(doc.Baselines["1234567"].Snapshot.Activities) != 1 {
		t.Fatalf("state=%#v err=%v", doc, err)
	}
}

func TestTimelineCatchUpBoundedIncompletePreservesBaselineAndRetries(t *testing.T) {
	dir := t.TempDir()
	client := &timelineScriptClient{pages: map[int]string{1: timelinePage(1, 1, timelineItem(100, "old"))}}
	a := &app{dir: dir, origin: "https://portal.example", client: client}
	initial, err := runCheckResult(t, a)
	if err != nil {
		t.Fatal(err)
	}
	before, err := a.checkStateStore("family").Load(checkProfile{ID: "family", Origin: a.origin})
	if err != nil {
		t.Fatal(err)
	}
	client.pages = map[int]string{}
	for page := 1; page <= timelineCatchUpPageLimit; page++ {
		client.pages[page] = timelinePage(page, timelineCatchUpPageLimit+1, timelineItem(200+page, fmt.Sprintf("page-%d", page)))
	}
	incomplete, err := runCheckResult(t, a)
	var exitErr *exitStatus
	if incomplete.Outcome != model.OutcomeIncomplete || !asExitStatus(err, &exitErr) || exitErr.code != 3 {
		t.Fatalf("incomplete=%#v err=%#v", incomplete, err)
	}
	if incomplete.Coverage[0].Continuity.Reason != "timeline_page_limit" || !incomplete.Guidance.Retryable {
		t.Fatalf("incomplete=%#v", incomplete)
	}
	after, err := a.checkStateStore("family").Load(checkProfile{ID: "family", Origin: a.origin})
	if err != nil {
		t.Fatal(err)
	}
	if after.Baselines["1234567"].CheckID != before.Baselines["1234567"].CheckID || after.LastSuccess.CheckID != initial.CheckID {
		t.Fatalf("baseline advanced after incomplete: before=%#v after=%#v", before.Baselines, after.Baselines)
	}
	client.pages = map[int]string{
		1: timelinePage(1, 2, timelineItem(201, "new")),
		2: timelinePage(2, 2, timelineItem(100, "old")),
	}
	recovered, err := runCheckResult(t, a)
	if err != nil || recovered.Outcome != model.OutcomeCompleteWithChanges || recovered.Changes.Count != 1 {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
}

func TestTimelineCatchUpRejectsChangingDuplicate(t *testing.T) {
	priorItem := model.Activity{ID: "unreachable"}
	client := &timelineScriptClient{pages: map[int]string{
		1: timelinePage(1, 2, timelineItem(200, "first")),
		2: timelinePage(2, 2, timelineItem(200, "changed")),
	}}
	runner := liveCheckRunner{app: &app{client: client}}
	run, err := runner.Check(context.Background(), checkRequest{CheckID: "check_test", Enrolments: []string{"1234567"}, Prior: map[string]checkstate.Baseline{"1234567": {Snapshot: model.StudentData{Student: model.Student{ID: "1234567"}}, TimelineBoundary: []string{priorItem.ID}}}})
	if err != nil || run.Result.Outcome != model.OutcomeIncomplete || run.Result.Coverage[0].Continuity.Reason != "timeline_source_changed" {
		t.Fatalf("run=%#v err=%v", run, err)
	}
}

func TestTimelineCatchUpStopsOnRepeatedPageContent(t *testing.T) {
	client := &timelineScriptClient{pages: map[int]string{
		1: timelinePage(1, 3, timelineItem(200, "same")),
		2: timelinePage(2, 3, timelineItem(200, "same")),
	}}
	runner := liveCheckRunner{app: &app{client: client}}
	run, err := runner.Check(context.Background(), checkRequest{CheckID: "check_test", Enrolments: []string{"1234567"}, Prior: map[string]checkstate.Baseline{"1234567": {Snapshot: model.StudentData{Student: model.Student{ID: "1234567"}}, TimelineBoundary: []string{"missing"}}}})
	if err != nil || run.Result.Outcome != model.OutcomeIncomplete || run.Result.Coverage[0].Continuity.Reason != "timeline_repeated_page" || len(client.requests) != 2 {
		t.Fatalf("run=%#v requests=%v err=%v", run, client.requests, err)
	}
}

func TestInitialTimelineBaselineRejectsConflictingDuplicate(t *testing.T) {
	client := &timelineScriptClient{pages: map[int]string{1: timelinePage(1, 1, timelineItem(100, "first"), timelineItem(100, "changed"))}}
	runner := liveCheckRunner{app: &app{client: client}}
	run, err := runner.Check(context.Background(), checkRequest{CheckID: "check_test", Enrolments: []string{"1234567"}, Prior: map[string]checkstate.Baseline{}})
	if err != nil || run.Result.Outcome != model.OutcomeIncomplete || run.Result.Coverage[0].Continuity.Reason != "timeline_source_changed" {
		t.Fatalf("run=%#v err=%v", run, err)
	}
}

func TestTimelineCatchUpStopsAtElapsedTimeBound(t *testing.T) {
	client := &timelineScriptClient{pages: map[int]string{1: timelinePage(1, 2, timelineItem(200, "new"))}}
	times := []time.Time{time.Unix(0, 0), time.Unix(0, 0).Add(timelineCatchUpTimeLimit)}
	runner := liveCheckRunner{app: &app{client: client}, now: func() time.Time {
		next := times[0]
		times = times[1:]
		return next
	}}
	run, err := runner.Check(context.Background(), checkRequest{CheckID: "check_test", Enrolments: []string{"1234567"}, Prior: map[string]checkstate.Baseline{"1234567": {Snapshot: model.StudentData{Student: model.Student{ID: "1234567"}}, TimelineBoundary: []string{"missing"}}}})
	if err != nil || run.Result.Outcome != model.OutcomeIncomplete || run.Result.Coverage[0].Continuity.Reason != "timeline_time_limit" || len(client.requests) != 1 {
		t.Fatalf("run=%#v requests=%v err=%v", run, client.requests, err)
	}
}

func TestTimelineCatchUpRevalidatesHeadBeforeCompleting(t *testing.T) {
	client := &shiftingHeadClient{timelineScriptClient: &timelineScriptClient{pages: map[int]string{
		1: timelinePage(1, 2, timelineItem(200, "new")),
		2: timelinePage(2, 2, timelineItem(100, "old")),
	}}}
	runner := liveCheckRunner{app: &app{client: client}}
	run, err := runner.Check(context.Background(), checkRequest{CheckID: "check_test", Enrolments: []string{"1234567"}, Prior: map[string]checkstate.Baseline{"1234567": {Snapshot: model.StudentData{Student: model.Student{ID: "1234567"}}, TimelineBoundary: []string{stableTimelineID("1234567", 100)}}}})
	if err != nil || run.Result.Outcome != model.OutcomeIncomplete || run.Result.Coverage[0].Continuity.Reason != "timeline_source_changed" || len(run.Observations) != 0 {
		t.Fatalf("run=%#v err=%v", run, err)
	}
}

func TestInitialTimelineBaselineReadsOnlyNewestPage(t *testing.T) {
	client := &timelineScriptClient{pages: map[int]string{1: timelinePage(1, 20, timelineItem(100, "existing"))}}
	runner := liveCheckRunner{app: &app{client: client}}
	run, err := runner.Check(context.Background(), checkRequest{CheckID: "check_test", Enrolments: []string{"1234567"}, Prior: map[string]checkstate.Baseline{}})
	if err != nil || run.Result.Outcome != model.OutcomeInitialBaseline || len(run.Result.Changes.Items) != 0 || len(client.requests) != 1 {
		t.Fatalf("run=%#v requests=%v err=%v", run, client.requests, err)
	}
}

func TestSyncProfileUsesReliableCheckContract(t *testing.T) {
	a := &app{dir: t.TempDir(), origin: "https://portal.example", client: &timelineScriptClient{pages: map[int]string{1: timelinePage(1, 3, timelineItem(100, "existing"))}}}
	data, err := captureStdout(t, func() error {
		return a.sync(context.Background(), []string{"--profile=family", "--student=1234567"})
	})
	var result checkResult
	if decodeErr := json.Unmarshal(data, &result); err != nil || decodeErr != nil || result.SchemaVersion != model.CheckSchemaVersion || result.Outcome != model.OutcomeInitialBaseline {
		t.Fatalf("result=%#v err=%v decode=%v", result, err, decodeErr)
	}
}

func TestProcessCheckCatchesUpPaginatedTimelineAndCommitsState(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "ednevnik")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	var catchUp atomic.Bool
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/grades":
			_, _ = w.Write([]byte(`<a class="flex-table-row" href="/grades/7654321/show?student=1234567"><div><strong class="d-block">Mathematics</strong></div></a>`))
		case "/absents":
			_, _ = w.Write([]byte(`<div class="categories-wrap"></div>`))
		case "/timeline-data":
			page, _ := strconv.Atoi(req.URL.Query().Get("page"))
			if !catchUp.Load() {
				_, _ = w.Write([]byte(timelinePage(1, 1, timelineItem(100, "old"))))
			} else if page == 1 {
				_, _ = w.Write([]byte(timelinePage(1, 2, timelineItem(103, "new-3"), timelineItem(102, "new-2"))))
			} else {
				_, _ = w.Write([]byte(timelinePage(2, 2, timelineItem(101, "new-1"), timelineItem(100, "old"))))
			}
		default:
			http.NotFound(w, req)
		}
	}))
	defer portal.Close()
	stateDir := t.TempDir()
	env := append(os.Environ(), "EDNEVNIK_TEST_ALLOW_HTTP_LOOPBACK=1", "EDNEVNIK_REQUEST_INTERVAL=0s", "EDNEVNIK_STATE_DIR="+stateDir, "EDNEVNIK_TEST_STATE_ROOT="+t.TempDir(), "EDNEVNIK_BASE_URL="+portal.URL)
	run := func(args ...string) checkResult {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%v: %v stderr=%s", args, err, stderr.String())
		}
		var result checkResult
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatalf("decode %q: %v", stdout.String(), err)
		}
		return result
	}
	if initial := run("check", "--profile=family", "--student=1234567"); initial.Outcome != model.OutcomeInitialBaseline {
		t.Fatalf("initial=%#v", initial)
	}
	catchUp.Store(true)
	result := run("check", "--profile=family", "--student=1234567")
	if result.Outcome != model.OutcomeCompleteWithChanges || result.Changes.Count != 3 || result.Coverage[0].Continuity.Reason != "timeline_overlap_established" {
		t.Fatalf("result=%#v", result)
	}
}

func stableTimelineID(studentID string, portalID int) string {
	page, err := parse.Timeline([]byte(timelinePage(1, 1, timelineItem(portalID, "old"))), studentID, 1)
	if err != nil {
		panic(err)
	}
	return page.Items[0].ID
}

func asExitStatus(err error, target **exitStatus) bool {
	if err == nil {
		return false
	}
	value, ok := err.(*exitStatus)
	if ok {
		*target = value
	}
	return ok
}
