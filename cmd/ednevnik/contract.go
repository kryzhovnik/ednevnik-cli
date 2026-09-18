package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

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
}

type checkRunner interface {
	Check(context.Context, checkRequest) (checkResult, error)
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

type invalidCheckStatusError struct{ err error }

func (e *invalidCheckStatusError) Error() string { return e.err.Error() }
func (e *invalidCheckStatusError) Unwrap() error { return e.err }

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
		var prior checkStatus
		statusErr := store.LoadSnapshot(a.checkStatusPath(*profile), &prior)
		if statusErr == nil && prior.LatestAttempt != nil && !prior.LatestAttempt.CompletedAt.IsZero() {
			elapsed := time.Since(prior.LatestAttempt.CompletedAt)
			if elapsed < minimum {
				return newContractError("", model.ReasonRefusalQuota, fmt.Errorf("minimum check interval is %s; retry in %s", minimum, (minimum-elapsed).Round(time.Second)), true, "Wait for the local interval or use --force; force still respects request budgets and server cooldowns.", 1)
			}
		} else if statusErr != nil && !os.IsNotExist(statusErr) {
			return newContractError("", model.ReasonInvalidState, statusErr, false, "Preserve and repair the existing status file.", 1)
		}
	}
	now := time.Now().UTC()
	checkID, err := newCheckID()
	if err != nil {
		return newContractError("", "io", err, true, "Retry the command.", 1)
	}
	req := checkRequest{CheckID: checkID, ProfileID: *profile, Enrolments: selected, StartedAt: now}
	runner := a.checker
	if runner == nil {
		runner = liveCheckRunner{app: a}
	}
	result, err := runner.Check(ctx, req)
	if err != nil {
		return a.failCheck(req, classifyCheckError(req.CheckID, err))
	}
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
	if err := validateCheckResult(result); err != nil {
		return a.failCheck(req, newContractError(req.CheckID, "invalid_state", err, false, "Do not consume this result; repair the check implementation or local state.", 1))
	}
	if err := a.recordCheckStatus(result); err != nil {
		var invalid *invalidCheckStatusError
		if errors.As(err, &invalid) {
			return newContractError(req.CheckID, model.ReasonInvalidState, errors.New("existing check status is invalid or belongs to another account/profile origin"), false, "Preserve the file and migrate or recover it explicitly.", 1)
		}
		return newContractError(req.CheckID, "io", err, true, "Check local state permissions and free space, then retry.", 1)
	}
	if err := output(result); err != nil {
		return err
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
	if err := a.recordCheckStatus(failed); err != nil {
		var invalid *invalidCheckStatusError
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

type liveCheckRunner struct{ app *app }

func (r liveCheckRunner) Check(ctx context.Context, req checkRequest) (checkResult, error) {
	coverage := make([]enrolmentCoverage, 0, len(req.Enrolments))
	for _, id := range req.Enrolments {
		current := enrolmentCoverage{EnrolmentID: id, Sections: []sectionCoverage{}, Continuity: continuityCoverage{State: "not_checked"}}
		body, err := r.app.get(ctx, "/grades?student="+id)
		if err != nil {
			return checkResult{}, withCurrentCoverage(liveReadFailure(err), coverage, current)
		}
		if _, err := parse.Subjects(body, id); err != nil {
			return checkResult{}, invalidSourceFailure("grade overview did not match the recognized source structure", coverage, current)
		}
		current.Sections = append(current.Sections, sectionCoverage{Name: "grades", State: "complete", Representation: "overview"})
		body, err = r.app.get(ctx, "/absents?student="+id)
		if err != nil {
			return checkResult{}, withCurrentCoverage(liveReadFailure(err), coverage, current)
		}
		if _, err := parse.Absences(body, id); err != nil {
			return checkResult{}, invalidSourceFailure("absence response did not match the recognized source structure", coverage, current)
		}
		current.Sections = append(current.Sections, sectionCoverage{Name: "absences", State: "complete", Representation: "current_records"})
		body, err = r.app.get(ctx, "/timeline-data?page=1&student="+id)
		if err != nil {
			return checkResult{}, withCurrentCoverage(liveReadFailure(err), coverage, current)
		}
		if _, err := parse.Timeline(body, id, 1); err != nil {
			return checkResult{}, invalidSourceFailure("timeline response did not match the recognized source structure", coverage, current)
		}
		current.Sections = append(current.Sections, sectionCoverage{Name: "timeline", State: "complete", Representation: "newest_page"})
		current.Continuity = continuityCoverage{State: "incomplete", Reason: "timeline_catch_up_not_implemented"}
		coverage = append(coverage, current)
	}
	return checkResult{
		Outcome: "incomplete", Coverage: coverage,
		Baseline: baselineReference{NewEnrolments: []string{}},
		Changes:  changeSummary{Items: []model.Change{}},
		Guidance: guidance{Retryable: false, Action: "No baseline was committed. Use legacy sync only for schema-v2 consumers; wait for continuity support before treating this check as complete."},
	}, nil
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

func (a *app) recordCheckStatus(result checkResult) error {
	if result.Profile.ID == "" || result.Profile.Origin == "" {
		return errors.New("check result has no account/profile origin namespace")
	}
	path := a.checkStatusPath(result.Profile.ID)
	var status checkStatus
	exists := true
	if err := store.LoadSnapshot(path, &status); err != nil {
		if os.IsNotExist(err) {
			exists = false
		} else {
			var syntaxErr *json.SyntaxError
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
				return &invalidCheckStatusError{err: err}
			}
			return err
		}
	}
	if exists {
		if err := validateCheckStatus(status, result.Profile.ID, result.Profile.Origin); err != nil {
			return &invalidCheckStatusError{err: err}
		}
	}
	status.SchemaVersion = checkSchemaVersion
	status.History = "latest_attempt_only"
	status.LatestAttempt = &result
	if result.Outcome == "initial_baseline" || result.Outcome == "complete_with_changes" || result.Outcome == "complete_without_changes" {
		status.LastSuccess = &result
	}
	return store.SaveSnapshot(path, status)
}

func validateCheckStatus(status checkStatus, profileID, origin string) error {
	if status.SchemaVersion != checkSchemaVersion || status.History != "latest_attempt_only" || status.LatestAttempt == nil {
		return errors.New("unsupported check status envelope")
	}
	validateBinding := func(result *checkResult) error {
		if result.Profile.ID != profileID || result.Profile.Origin != origin || result.CheckID == "" {
			return errors.New("check status account/profile origin mismatch")
		}
		return nil
	}
	if err := validateBinding(status.LatestAttempt); err != nil {
		return err
	}
	switch status.LatestAttempt.Outcome {
	case model.OutcomeInitialBaseline, model.OutcomeCompleteWithChanges, model.OutcomeCompleteWithoutChanges, model.OutcomeIncomplete, model.OutcomeFailed:
	default:
		return errors.New("unsupported latest-attempt outcome")
	}
	if status.LatestAttempt.Outcome != model.OutcomeFailed {
		if err := validateCheckResult(*status.LatestAttempt); err != nil {
			return err
		}
	}
	if status.LatestAttempt.Outcome == model.OutcomeFailed && (status.LatestAttempt.Failure == nil || status.LatestAttempt.Failure.Reason == "") {
		return errors.New("failed latest attempt has no reason")
	}
	if status.LastSuccess != nil {
		if err := validateBinding(status.LastSuccess); err != nil {
			return err
		}
		switch status.LastSuccess.Outcome {
		case model.OutcomeInitialBaseline, model.OutcomeCompleteWithChanges, model.OutcomeCompleteWithoutChanges:
		default:
			return errors.New("last success has a non-success outcome")
		}
		if err := validateCheckResult(*status.LastSuccess); err != nil {
			return err
		}
	}
	return nil
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
