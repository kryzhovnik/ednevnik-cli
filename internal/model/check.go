package model

import "time"

const CheckSchemaVersion = 3

const (
	OutcomeInitialBaseline        = "initial_baseline"
	OutcomeCompleteWithChanges    = "complete_with_changes"
	OutcomeCompleteWithoutChanges = "complete_without_changes"
	OutcomeIncomplete             = "incomplete"
	OutcomeFailed                 = "failed"
)

const (
	ReasonAuthenticationProvider = "authentication_provider"
	ReasonRefusalQuota           = "refusal_quota"
	ReasonInvalidSource          = "invalid_source"
	ReasonInvalidState           = "invalid_state"
	ReasonConcurrency            = "concurrency"
	ReasonCancelled              = "cancelled"
	ReasonIO                     = "io"
	ReasonInvalidArgument        = "invalid_argument"
)

type CheckResult struct {
	SchemaVersion int                 `json:"schema_version"`
	CheckID       string              `json:"check_id"`
	Outcome       string              `json:"outcome"`
	Profile       CheckProfile        `json:"profile"`
	Requested     []string            `json:"requested_enrolments"`
	Coverage      []EnrolmentCoverage `json:"coverage"`
	StartedAt     time.Time           `json:"started_at"`
	CompletedAt   time.Time           `json:"completed_at"`
	Baseline      BaselineReference   `json:"baseline"`
	Changes       ChangeSummary       `json:"changes"`
	Guidance      Guidance            `json:"guidance"`
	Failure       *FailureSummary     `json:"failure,omitempty"`
}

type CheckProfile struct {
	ID     string `json:"id"`
	Origin string `json:"origin"`
}
type EnrolmentCoverage struct {
	EnrolmentID string             `json:"enrolment_id"`
	Sections    []SectionCoverage  `json:"sections"`
	Continuity  ContinuityCoverage `json:"continuity"`
}
type SectionCoverage struct {
	Name               string `json:"name"`
	State              string `json:"state"`
	Representation     string `json:"representation"`
	FirstPage          int    `json:"first_page,omitempty"`
	LastPage           int    `json:"last_page,omitempty"`
	PageCount          int    `json:"page_count,omitempty"`
	RecordsInspected   int    `json:"records_inspected,omitempty"`
	CorrectionCoverage string `json:"correction_coverage,omitempty"`
}
type ContinuityCoverage struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}
type BaselineReference struct {
	ID            string   `json:"id,omitempty"`
	PreviousID    string   `json:"previous_id,omitempty"`
	NewEnrolments []string `json:"new_enrolments"`
}
type ChangeSummary struct {
	Count int      `json:"count"`
	Items []Change `json:"items"`
}

// RetainedEvent is the durable ordering envelope for a committed transition.
// Change semantics are intentionally supplied by the reconciliation module.
type RetainedEvent struct {
	ID       string `json:"id"`
	Sequence uint64 `json:"sequence"`
	Revision uint64 `json:"revision"`
	CheckID  string `json:"check_id"`
	Change   Change `json:"change"`
}
type Guidance struct {
	Retryable  bool       `json:"retryable"`
	RetryAfter *time.Time `json:"retry_after,omitempty"`
	Action     string     `json:"action"`
}
type FailureSummary struct {
	Reason string `json:"reason"`
}
type CheckStatus struct {
	SchemaVersion int          `json:"schema_version"`
	History       string       `json:"history"`
	LatestAttempt *CheckResult `json:"latest_attempt"`
	LastSuccess   *CheckResult `json:"last_success"`
}
type ContractError struct {
	SchemaVersion int                 `json:"schema_version"`
	CheckID       string              `json:"check_id,omitempty"`
	Outcome       string              `json:"outcome"`
	Reason        string              `json:"reason"`
	Message       string              `json:"message"`
	Guidance      Guidance            `json:"guidance"`
	Profile       *CheckProfile       `json:"profile,omitempty"`
	Requested     []string            `json:"requested_enrolments,omitempty"`
	Coverage      []EnrolmentCoverage `json:"coverage,omitempty"`
	OccurredAt    time.Time           `json:"occurred_at"`
}
