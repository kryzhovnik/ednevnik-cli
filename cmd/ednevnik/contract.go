package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/kryzhovnik/ednevnik/internal/checkstate"
	"github.com/kryzhovnik/ednevnik/internal/client"
	"github.com/kryzhovnik/ednevnik/internal/coordination"
	"github.com/kryzhovnik/ednevnik/internal/model"
	"github.com/kryzhovnik/ednevnik/internal/parse"
	"github.com/kryzhovnik/ednevnik/internal/store"
)

const checkSchemaVersion = model.CheckSchemaVersion

type checkResult = model.CheckResult
type checkProfile = model.CheckProfile
type enrolmentCoverage = model.EnrolmentCoverage
type sectionCoverage = model.SectionCoverage
type continuityCoverage = model.ContinuityCoverage
type baselineReference = model.BaselineReference
type changeSummary = model.ChangeSummary
type guidance = model.Guidance
type checkStatus = model.CheckStatus
type contractError = model.ContractError

type checkRequest struct {
	CheckID    string
	ProfileID  string
	Enrolments []string
	StartedAt  time.Time
	Prior      map[string]checkstate.Baseline
}

type checkRunner interface {
	Check(context.Context, checkRequest) (checkRun, error)
}

type checkRun struct {
	Result       checkResult
	Observations []checkstate.Observation
}

type commandError struct {
	code int
	body contractError
}

// checkFailure is the error seam for fetch, validation, coordination, and
// storage implementations. Reason is part of the public contract.
type checkFailure struct {
	Reason     string
	Err        error
	Retryable  bool
	RetryAfter *time.Time
	Action     string
	Coverage   []enrolmentCoverage
}

func (e *checkFailure) Error() string { return e.Err.Error() }
func (e *checkFailure) Unwrap() error { return e.Err }

func (e *commandError) Error() string { return e.body.Message }

type exitStatus struct{ code int }

func (e *exitStatus) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func (a *app) check(ctx context.Context, args []string) error {
	fs := commandFlagSet("check")
	profile := fs.String("profile", "", "configured account/profile namespace")
	var enrolments stringList
	fs.Var(&enrolments, "student", "enrolment ID; repeat for several enrolments")
	force := fs.Bool("force", false, "bypass only the local minimum check interval")
	if err := fs.Parse(args); err != nil {
		return newContractError("", "invalid_argument", err, false, "Fix the command arguments and retry.", 2)
	}
	if *profile == "" {
		return newContractError("", "invalid_argument", errors.New("check requires --profile"), false, "Select the configured account/profile namespace.", 2)
	}
	if err := validateNamespace(*profile, "profile"); err != nil {
		return newContractError("", "invalid_argument", err, false, "Use lowercase letters, digits, and underscores for the profile namespace.", 2)
	}
	if len(enrolments) == 0 {
		return newContractError("", "invalid_argument", errors.New("check requires at least one --student"), false, "Pass each intended enrolment with --student.", 2)
	}
	seen := make(map[string]bool)
	selected := enrolments[:0]
	for _, id := range enrolments {
		if err := validateStudentID(id); err != nil {
			return newContractError("", "invalid_argument", err, false, "Use the portal enrolment identifier shown by `ednevnik students`.", 2)
		}
		if !seen[id] {
			seen[id] = true
			selected = append(selected, id)
		}
	}
	sort.Strings(selected)
	minimum, err := configuredMinimumCheckInterval()
	if err != nil {
		return newContractError("", model.ReasonInvalidArgument, err, false, "Correct EDNEVNIK_MIN_CHECK_INTERVAL.", 2)
	}
	if !*force {
		priorState, statusErr := a.checkStateStore(*profile).Load(checkProfile{ID: *profile, Origin: a.origin})
		if statusErr == nil && priorState.LatestAttempt != nil && !priorState.LatestAttempt.CompletedAt.IsZero() {
			elapsed := time.Since(priorState.LatestAttempt.CompletedAt)
			if elapsed < minimum {
				return newContractError("", model.ReasonRefusalQuota, fmt.Errorf("minimum check interval is %s; retry in %s", minimum, (minimum-elapsed).Round(time.Second)), true, "Wait for the local interval or use --force; force still respects request budgets and server cooldowns.", 1)
			}
		} else if statusErr != nil && !errors.Is(statusErr, checkstate.ErrAbsent) {
			return newContractError("", model.ReasonInvalidState, statusErr, false, "Preserve the state. Archive legacy schema-v2 files before establishing a schema-v3 baseline, or repair the named schema-v3 state file.", 1)
		}
	}
	now := time.Now().UTC()
	checkID, err := newCheckID()
	if err != nil {
		return newContractError("", "io", err, true, "Retry the command.", 1)
	}
	req := checkRequest{CheckID: checkID, ProfileID: *profile, Enrolments: selected, StartedAt: now}
	stateStore := a.checkStateStore(*profile)
	prior, loadErr := stateStore.Load(checkProfile{ID: *profile, Origin: a.origin})
	if loadErr == nil {
		req.Prior = prior.Baselines
	} else if !errors.Is(loadErr, checkstate.ErrAbsent) {
		return newContractError(req.CheckID, model.ReasonInvalidState, loadErr, false, "Preserve the state. For legacy schema-v2 data, archive it and establish a schema-v3 baseline; otherwise repair the named state file.", 1)
	} else {
		req.Prior = map[string]checkstate.Baseline{}
	}
	runner := a.checker
	if runner == nil {
		runner = liveCheckRunner{app: a}
	}
	run, err := runner.Check(ctx, req)
	if err != nil {
		return a.failCheck(req, classifyCheckError(req.CheckID, err))
	}
	result := run.Result
	result.SchemaVersion = checkSchemaVersion
	result.CheckID = req.CheckID
	result.Profile = checkProfile{ID: req.ProfileID, Origin: a.origin}
	result.Requested = append([]string(nil), req.Enrolments...)
	result.StartedAt = req.StartedAt
	if result.CompletedAt.IsZero() {
		result.CompletedAt = time.Now().UTC()
	}
	if result.Baseline.NewEnrolments == nil {
		result.Baseline.NewEnrolments = []string{}
	}
	if result.Changes.Items == nil {
		result.Changes.Items = []model.Change{}
	}
	if result.Changes.Count != len(result.Changes.Items) {
		return a.failCheck(req, newContractError(req.CheckID, model.ReasonInvalidState, errors.New("change count does not match items"), false, "Repair the check implementation; no successful baseline was committed.", 1))
	}
	if err := validateCheckResult(result); err != nil {
		return a.failCheck(req, newContractError(req.CheckID, "invalid_state", err, false, "Do not consume this result; repair the check implementation or local state.", 1))
	}
	committed, err := stateStore.Commit(result.Profile, result, run.Observations)
	if err != nil {
		var stateInvalid *checkstate.InvalidError
		if errors.As(err, &stateInvalid) {
			return newContractError(req.CheckID, model.ReasonInvalidState, errors.New("existing check status is invalid or belongs to another account/profile origin"), false, "Preserve the file and migrate or recover it explicitly.", 1)
		}
		var commitErr *checkstate.CommitError
		if errors.As(err, &commitErr) && commitErr.Committed {
			return newContractError(req.CheckID, model.ReasonIO, errors.New("check committed but durable-directory confirmation or output did not complete"), true, "Inspect local status for this check ID before retrying; committed retained events remain available.", 1)
		}
		return newContractError(req.CheckID, "io", err, true, "Check local state permissions and free space, then retry.", 1)
	}
	result = *committed.LatestAttempt
	if err := output(result); err != nil {
		return newContractError(req.CheckID, model.ReasonIO, errors.New("check committed but result output was interrupted"), true, "Inspect local status for this check ID before retrying; committed retained events remain available.", 1)
	}
	if result.Outcome == "incomplete" {
		return &exitStatus{code: 3}
	}
	return nil
}

func (a *app) failCheck(req checkRequest, ce *commandError) error {
	failed := failedResult(req, ce.body)
	failed.Profile.Origin = a.origin
	ce.body.Profile = &failed.Profile
	ce.body.Requested = failed.Requested
	ce.body.Coverage = failed.Coverage
	if _, err := a.checkStateStore(req.ProfileID).Commit(failed.Profile, failed, nil); err != nil {
		var invalid *checkstate.InvalidError
		if errors.As(err, &invalid) {
			return newContractError(req.CheckID, model.ReasonInvalidState, errors.New("existing check status is invalid or belongs to another account/profile origin"), false, "Preserve the file and migrate or recover it explicitly.", 1)
		}
		return newContractError(req.CheckID, "io", err, true, "Preserve the prior state and repair local storage before retrying.", 1)
	}
	return ce
}

func validateCheckResult(result checkResult) error {
	switch result.Outcome {
	case model.OutcomeIncomplete:
		if result.Baseline.ID != "" || result.Baseline.PreviousID != "" || result.Changes.Count != 0 || len(result.Changes.Items) != 0 {
			return errors.New("incomplete result claims a committed baseline or changes")
		}
		return validateRequestedCoverage(result, false)
	case model.OutcomeInitialBaseline, model.OutcomeCompleteWithChanges, model.OutcomeCompleteWithoutChanges:
	default:
		return fmt.Errorf("unsupported check outcome %q", result.Outcome)
	}
	if result.Baseline.ID == "" {
		return errors.New("complete result has no committed baseline reference")
	}
	if result.Outcome == model.OutcomeCompleteWithChanges && result.Changes.Count < 1 {
		return errors.New("changed result has no changes")
	}
	if result.Outcome == model.OutcomeCompleteWithoutChanges && result.Changes.Count != 0 {
		return errors.New("unchanged result contains changes")
	}
	return validateRequestedCoverage(result, true)
}

func validateRequestedCoverage(result checkResult, requireComplete bool) error {
	byID := make(map[string]enrolmentCoverage, len(result.Coverage))
	for _, coverage := range result.Coverage {
		if _, exists := byID[coverage.EnrolmentID]; exists {
			return fmt.Errorf("duplicate coverage for enrolment %s", coverage.EnrolmentID)
		}
		byID[coverage.EnrolmentID] = coverage
	}
	for _, id := range result.Requested {
		coverage, ok := byID[id]
		if !ok {
			return fmt.Errorf("complete result has no coverage for enrolment %s", id)
		}
		sections := make(map[string]string, len(coverage.Sections))
		for _, section := range coverage.Sections {
			sections[section.Name] = section.State
		}
		for _, required := range []string{"grades", "absences", "timeline"} {
			if requireComplete && sections[required] != "complete" {
				return fmt.Errorf("complete result lacks complete %s coverage for enrolment %s", required, id)
			}
		}
		if requireComplete && coverage.Continuity.State != "complete" {
			return fmt.Errorf("complete result lacks continuity for enrolment %s", id)
		}
	}
	for id := range byID {
		found := false
		for _, requested := range result.Requested {
			if requested == id {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("coverage contains unrequested enrolment %s", id)
		}
	}
	return nil
}

type liveCheckRunner struct {
	app *app
	now func() time.Time
}

const (
	// Seven older pages plus one head revalidation keep catch-up within the
	// documented allowance of eight requests beyond the normal newest page.
	timelineCatchUpPageLimit = 8
	timelineCatchUpTimeLimit = 2 * time.Minute
)

func (r liveCheckRunner) Check(ctx context.Context, req checkRequest) (checkRun, error) {
	coverage := make([]enrolmentCoverage, 0, len(req.Enrolments))
	observations := make([]checkstate.Observation, 0, len(req.Enrolments))
	changes := []model.Change{}
	newEnrolments := []string{}
	complete := true
	for _, id := range req.Enrolments {
		current := enrolmentCoverage{EnrolmentID: id, Sections: []sectionCoverage{}, Continuity: continuityCoverage{State: "not_checked"}}
		body, err := r.app.get(ctx, "/grades?student="+id)
		if err != nil {
			return checkRun{}, withCurrentCoverage(liveReadFailure(err), coverage, current)
		}
		subjects, err := parse.Subjects(body, id)
		if err != nil {
			return checkRun{}, invalidSourceFailure("grade overview did not match the recognized source structure", coverage, current)
		}
		current.Sections = append(current.Sections, sectionCoverage{Name: "grades", State: "complete", Representation: "overview"})
		body, err = r.app.get(ctx, "/absents?student="+id)
		if err != nil {
			return checkRun{}, withCurrentCoverage(liveReadFailure(err), coverage, current)
		}
		absences, err := parse.Absences(body, id)
		if err != nil {
			return checkRun{}, invalidSourceFailure("absence response did not match the recognized source structure", coverage, current)
		}
		current.Sections = append(current.Sections, sectionCoverage{Name: "absences", State: "complete", Representation: "current_records"})
		prior, known := req.Prior[id]
		timeline, err := r.readTimeline(ctx, id, prior, known)
		if err != nil {
			return checkRun{}, withCurrentCoverage(err, coverage, current)
		}
		current.Sections = append(current.Sections, timeline.Section)
		current.Continuity = timeline.Continuity
		snapshot := model.StudentData{Student: model.Student{ID: id}, Subjects: subjects, Grades: []model.Grade{}, Absences: absences, Activities: timeline.Items}
		if !timeline.Complete {
			complete = false
		} else if !known {
			newEnrolments = append(newEnrolments, id)
		} else {
			diff := store.Diff(model.Snapshot{SchemaVersion: model.SchemaVersion, Students: []model.StudentData{prior.Snapshot}}, model.Snapshot{SchemaVersion: model.SchemaVersion, Students: []model.StudentData{snapshot}})
			changes = append(changes, filterTimelineAdditions(diff.Items, timeline.NewIDs)...)
		}
		observations = append(observations, checkstate.Observation{EnrolmentID: id, Snapshot: snapshot, TimelineBoundary: timeline.Boundary})
		coverage = append(coverage, current)
	}
	if !complete {
		return checkRun{Result: checkResult{Outcome: model.OutcomeIncomplete, Coverage: coverage, Baseline: baselineReference{NewEnrolments: []string{}}, Changes: changeSummary{Items: []model.Change{}}, Guidance: guidance{Retryable: true, Action: "The last complete baseline remains committed. Retry catch-up, or explicitly rebaseline if the portal can no longer expose the continuity boundary."}}}, nil
	}
	outcome := model.OutcomeCompleteWithoutChanges
	if len(req.Prior) == 0 {
		outcome = model.OutcomeInitialBaseline
	} else if len(changes) > 0 {
		outcome = model.OutcomeCompleteWithChanges
	}
	return checkRun{Result: checkResult{Outcome: outcome, Coverage: coverage, Baseline: baselineReference{ID: "baseline_" + strings.TrimPrefix(req.CheckID, "check_"), NewEnrolments: newEnrolments}, Changes: changeSummary{Count: len(changes), Items: changes}, Guidance: guidance{Action: "Committed retained events remain local until consumer replay is available."}}, Observations: observations}, nil
}

func filterTimelineAdditions(changes []model.Change, newIDs map[string]bool) []model.Change {
	filtered := make([]model.Change, 0, len(changes))
	for _, change := range changes {
		if change.Kind != "activity_added" || newIDs[change.RecordID] {
			filtered = append(filtered, change)
		}
	}
	return filtered
}

type timelineRead struct {
	Items      []model.Activity
	Boundary   []string
	NewIDs     map[string]bool
	Section    sectionCoverage
	Continuity continuityCoverage
	Complete   bool
}

func (r liveCheckRunner) readTimeline(ctx context.Context, enrolmentID string, prior checkstate.Baseline, known bool) (timelineRead, error) {
	started := r.nowTime()
	catchCtx, cancel := context.WithTimeout(ctx, timelineCatchUpTimeLimit)
	defer cancel()
	items := []model.Activity{}
	seen := map[string]model.Activity{}
	newIDs := map[string]bool{}
	seenPages := [][]model.Activity{}
	firstBoundary := []string{}
	firstPage := []model.Activity{}
	lastPage := 0
	pageNumber := 1
	for {
		if pageNumber > timelineCatchUpPageLimit {
			return incompleteTimeline(items, "timeline_page_limit", len(seenPages)), nil
		}
		if pageNumber > 1 && r.nowTime().Sub(started) >= timelineCatchUpTimeLimit {
			return incompleteTimeline(items, "timeline_time_limit", len(seenPages)), nil
		}
		body, err := r.app.get(catchCtx, fmt.Sprintf("/timeline-data?page=%d&student=%s", pageNumber, enrolmentID))
		if err != nil {
			if errors.Is(catchCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return incompleteTimeline(items, "timeline_time_limit", len(seenPages)), nil
			}
			return timelineRead{}, liveReadFailure(err)
		}
		page, err := parse.Timeline(body, enrolmentID, pageNumber)
		if err != nil {
			return timelineRead{}, &checkFailure{Reason: model.ReasonInvalidSource, Err: errors.New("timeline response did not match the recognized source structure"), Action: "Keep the last successful baseline and inspect portal compatibility before retrying."}
		}
		pageIDs := activityIDs(page.Items)
		if pageNumber == 1 {
			firstBoundary = pageIDs
			firstPage = append([]model.Activity{}, page.Items...)
			lastPage = page.LastPage
		} else if page.LastPage != lastPage {
			return incompleteTimeline(items, "timeline_source_changed", len(seenPages)+1), nil
		}
		for _, previousPage := range seenPages {
			if reflect.DeepEqual(previousPage, page.Items) {
				return incompleteTimeline(items, "timeline_repeated_page", len(seenPages)+1), nil
			}
		}
		seenPages = append(seenPages, append([]model.Activity{}, page.Items...))
		for _, item := range page.Items {
			if previous, exists := seen[item.ID]; exists {
				if !reflect.DeepEqual(previous, item) {
					return incompleteTimeline(items, "timeline_source_changed", len(seenPages)), nil
				}
				continue
			}
			seen[item.ID] = item
			items = append(items, item)
		}
		if pageNumber == 1 && !known && (len(page.Items) > 0 || page.NextPage == nil) {
			return timelineRead{Items: items, Boundary: pageIDs, NewIDs: newIDs, Section: timelineSection("complete", "recent_baseline_page", len(seenPages), len(items)), Continuity: continuityCoverage{State: "complete", Reason: "recent_baseline_established"}, Complete: true}, nil
		}
		boundary := activityIDs(page.Items)
		overlaps := hasTimelineOverlap(prior.TimelineBoundary, boundary)
		if overlaps {
			markNewActivityPrefix(newIDs, page.Items, prior.TimelineBoundary)
			return r.completeStableTimeline(ctx, catchCtx, enrolmentID, items, firstPage, firstBoundary, newIDs, lastPage, started, len(seenPages), "caught_up_pages", "timeline_overlap_established")
		}
		for _, item := range page.Items {
			newIDs[item.ID] = true
		}
		if len(prior.TimelineBoundary) == 0 && len(boundary) == 0 && page.NextPage == nil {
			return r.completeStableTimeline(ctx, catchCtx, enrolmentID, items, firstPage, firstBoundary, newIDs, lastPage, started, len(seenPages), "caught_up_to_source_boundary", "timeline_source_boundary_established")
		}
		if page.NextPage == nil {
			if !known {
				return r.completeStableTimeline(ctx, catchCtx, enrolmentID, items, firstPage, firstBoundary, newIDs, lastPage, started, len(seenPages), "recent_baseline_pages", "recent_baseline_established")
			}
			if len(prior.TimelineBoundary) == 0 {
				return r.completeStableTimeline(ctx, catchCtx, enrolmentID, items, firstPage, firstBoundary, newIDs, lastPage, started, len(seenPages), "caught_up_to_source_boundary", "timeline_source_boundary_established")
			}
			return incompleteTimeline(items, "timeline_gap", len(seenPages)), nil
		}
		pageNumber = *page.NextPage
	}
}

func (r liveCheckRunner) nowTime() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r liveCheckRunner) completeStableTimeline(parentCtx, catchCtx context.Context, enrolmentID string, items, firstPage []model.Activity, boundary []string, newIDs map[string]bool, lastPage int, started time.Time, inspectedPages int, representation, reason string) (timelineRead, error) {
	if r.nowTime().Sub(started) >= timelineCatchUpTimeLimit {
		return incompleteTimeline(items, "timeline_time_limit", inspectedPages), nil
	}
	body, err := r.app.get(catchCtx, "/timeline-data?page=1&student="+enrolmentID)
	if err != nil {
		if errors.Is(catchCtx.Err(), context.DeadlineExceeded) && parentCtx.Err() == nil {
			return incompleteTimeline(items, "timeline_time_limit", inspectedPages), nil
		}
		return timelineRead{}, liveReadFailure(err)
	}
	head, err := parse.Timeline(body, enrolmentID, 1)
	if err != nil {
		return timelineRead{}, &checkFailure{Reason: model.ReasonInvalidSource, Err: errors.New("timeline head revalidation did not match the recognized source structure"), Action: "Keep the last successful baseline and inspect portal compatibility before retrying."}
	}
	if head.LastPage != lastPage || !reflect.DeepEqual(head.Items, firstPage) {
		return incompleteTimeline(items, "timeline_source_changed", inspectedPages), nil
	}
	if r.nowTime().Sub(started) >= timelineCatchUpTimeLimit {
		return incompleteTimeline(items, "timeline_time_limit", inspectedPages), nil
	}
	return timelineRead{Items: items, Boundary: boundary, NewIDs: newIDs, Section: timelineSection("complete", representation, inspectedPages, len(items)), Continuity: continuityCoverage{State: "complete", Reason: reason}, Complete: true}, nil
}

func markNewActivityPrefix(newIDs map[string]bool, items []model.Activity, priorBoundary []string) {
	known := make(map[string]bool, len(priorBoundary))
	for _, id := range priorBoundary {
		known[id] = true
	}
	for _, item := range items {
		if known[item.ID] {
			return
		}
		newIDs[item.ID] = true
	}
}

func incompleteTimeline(items []model.Activity, reason string, inspectedPages int) timelineRead {
	return timelineRead{Items: items, Section: timelineSection("incomplete", "bounded_catch_up", inspectedPages, len(items)), Continuity: continuityCoverage{State: "incomplete", Reason: reason}}
}

func timelineSection(state, representation string, inspectedPages, records int) sectionCoverage {
	return sectionCoverage{Name: "timeline", State: state, Representation: representation, FirstPage: 1, LastPage: inspectedPages, PageCount: inspectedPages, RecordsInspected: records, CorrectionCoverage: "inspected_timeline_pages_only"}
}

func activityIDs(items []model.Activity) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	sort.Strings(ids)
	return ids
}
func hasTimelineOverlap(a, b []string) bool {
	seen := map[string]bool{}
	for _, id := range a {
		seen[id] = true
	}
	for _, id := range b {
		if seen[id] {
			return true
		}
	}
	return false
}

func liveReadFailure(err error) error {
	if errors.Is(err, client.ErrResponseTooLarge) {
		return &checkFailure{Reason: model.ReasonInvalidSource, Err: errors.New("portal response exceeded the size limit"), Action: "Keep the last successful baseline and inspect the failing coverage before retrying."}
	}
	if errors.Is(err, client.ErrNotAuthenticated) {
		return &checkFailure{Reason: "authentication_provider", Err: err, Action: "Select a supported credential provider and authenticate the configured profile."}
	}
	var httpErr *client.HTTPError
	if errors.As(err, &httpErr) {
		if httpErr.StatusCode == 429 || httpErr.StatusCode == 503 {
			retryAt := time.Now().UTC().Add(httpErr.RetryAfter)
			return &checkFailure{Reason: model.ReasonRefusalQuota, Err: err, Retryable: true, RetryAfter: &retryAt, Action: "Respect the persisted server cooldown before retrying."}
		}
		if httpErr.StatusCode == 401 || httpErr.StatusCode == 403 {
			return &checkFailure{Reason: model.ReasonAuthenticationProvider, Err: err, Action: "Authenticate the selected account once; do not retry the rejected credentials automatically."}
		}
	}
	var refusal *coordination.Refusal
	if errors.As(err, &refusal) {
		retryAt := refusal.RetryAt
		return &checkFailure{Reason: model.ReasonRefusalQuota, Err: err, Retryable: true, RetryAfter: &retryAt, Action: "Wait until the reported retry time; force does not bypass request budgets or server cooldowns."}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &checkFailure{Reason: model.ReasonCancelled, Err: err, Retryable: true, Action: "Retry as a new check after the cancellation or deadline condition is resolved."}
	}
	return err
}

func withCoverage(err error, coverage []enrolmentCoverage) error {
	var failure *checkFailure
	if errors.As(err, &failure) {
		failure.Coverage = append([]enrolmentCoverage(nil), coverage...)
		return failure
	}
	return &checkFailure{Reason: "io", Err: err, Retryable: true, Action: "Retry after checking network availability.", Coverage: append([]enrolmentCoverage(nil), coverage...)}
}

func withCurrentCoverage(err error, coverage []enrolmentCoverage, current enrolmentCoverage) error {
	return withCoverage(err, append(append([]enrolmentCoverage(nil), coverage...), current))
}

func invalidSourceFailure(message string, coverage []enrolmentCoverage, current enrolmentCoverage) error {
	return &checkFailure{Reason: "invalid_source", Err: errors.New(message), Action: "Keep the last successful baseline and inspect portal compatibility before retrying.", Coverage: append(append([]enrolmentCoverage(nil), coverage...), current)}
}

func failedResult(req checkRequest, e contractError) checkResult {
	coverage := e.Coverage
	if coverage == nil {
		coverage = []enrolmentCoverage{}
	}
	return checkResult{SchemaVersion: checkSchemaVersion, CheckID: req.CheckID, Outcome: "failed", Profile: checkProfile{ID: req.ProfileID}, Requested: req.Enrolments, Coverage: coverage, StartedAt: req.StartedAt, CompletedAt: e.OccurredAt, Baseline: baselineReference{NewEnrolments: []string{}}, Changes: changeSummary{Items: []model.Change{}}, Guidance: e.Guidance, Failure: &model.FailureSummary{Reason: e.Reason}}
}

func newCheckID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "check_" + hex.EncodeToString(raw), nil
}

func newContractError(checkID, reason string, err error, retryable bool, action string, code int) *commandError {
	return &commandError{code: code, body: contractError{SchemaVersion: checkSchemaVersion, CheckID: checkID, Outcome: "failed", Reason: reason, Message: err.Error(), Guidance: guidance{Retryable: retryable, Action: action}, OccurredAt: time.Now().UTC()}}
}

func classifyCheckError(checkID string, err error) *commandError {
	var failure *checkFailure
	if errors.As(err, &failure) {
		result := newContractError(checkID, failure.Reason, failure.Err, failure.Retryable, failure.Action, 1)
		result.body.Guidance.RetryAfter = failure.RetryAfter
		result.body.Coverage = failure.Coverage
		return result
	}
	reason, retryable, action := "io", true, "Retry after checking network and local I/O availability."
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		reason, action = "cancelled", "Retry the whole command; no baseline was committed."
	}
	return newContractError(checkID, reason, err, retryable, action, 1)
}
