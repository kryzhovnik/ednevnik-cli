# Automation contract

The beta automation interface is `ednevnik check`. It emits schema version 3
JSON on standard output. Diagnostics and structured command errors use standard
error. Existing `sync`, `changes`, and their stored files remain schema version
2 during the staged transition. They are not silently interpreted as version 3.

```sh
ednevnik check --profile family --student 1234567 --student 2345678
ednevnik status --profile family
```

`--profile` plus the canonical HTTPS portal origin is the configured
parent-account namespace. It is not a username
and must not contain a secret. A student ID identifies an enrolment (the
child-school-class-school-year association), not a permanent child. Repeated
selections are deduplicated. Selecting a subset means “observe only these
enrolments”; it never implies removal of unselected data.

## Results and exit status

A result contains `schema_version`, `check_id`, `profile`,
`requested_enrolments`, per-enrolment `coverage`, `started_at`, `completed_at`,
`baseline`, `changes`, and `guidance`. Every required section is named in
coverage. Grade overview, individual grade details, current absence records,
and timeline pages are different representations. Timeline continuity is a
separate coverage dimension.

`check_id` is a random opaque identifier; consumers must not infer time or
ordering from it. The profile contains the secret-free canonical portal origin.
Existing status for the same profile but a different origin or schema is
refused as `invalid_state` and preserved for explicit recovery.

The stable outcomes are:

| Outcome | Meaning | Exit |
| --- | --- | --- |
| `initial_baseline` | All requested coverage passed; prior records establish the baseline and are not new events. | 0 |
| `complete_with_changes` | Coverage and continuity passed and changes were committed. | 0 |
| `complete_without_changes` | Coverage and continuity passed with no changes. | 0 |
| `incomplete` | Some requested coverage or continuity was not established; no successful baseline is claimed. | 3 |
| `failed` | The attempt could not produce a check result. | 1, or 2 for command usage. |

Incomplete results are JSON on standard output. Failed attempts are a JSON
error object on standard error with `outcome: "failed"`, profile, requested
enrolments, completed partial coverage, a stable `reason`, and
retry/recovery guidance. Scripts must not parse `message`. Stable reasons are
`authentication_provider`, `refusal_quota`, `invalid_source`, `invalid_state`,
`concurrency`, `cancelled`, `io`, and `invalid_argument`. Exit 2 means the
invocation must be corrected. Exit 1 means an operational failure. `check` is
always noninteractive. In this intermediate implementation it does
not invoke Keychain or any other implicit credential lookup. The authentication
slice will add explicit provider selection. Whole-command cancellation is
`cancelled`; retrying starts a new check and cannot make the
cancelled attempt successful.

The current live adapter validates grade overview, current absences, and the
newest timeline page through the real parsers. Catch-up continuity is not yet
implemented, so it returns `incomplete` with
`timeline_catch_up_not_implemented` and does not commit a schema-v3 baseline.
This is intentional: HTTP success and a parsed newest page do not establish
continuity. Scripted adapters exercise all complete outcomes while the fetch,
validation, and atomic-state slices are developed behind the same high-level
check interface.

`status --profile NAME` is local-only. It never contacts the portal or loads credentials. It
reports `latest_attempt` separately from `last_success`. Until durable history
lands, `history` is `latest_attempt_only`; with only legacy schema-v2 state it is
`unavailable`. Plain `status` and `status --consumer` preserve their schema-v2
shape during the transition.

## Downstream contracts

A baseline ID names one coherently committed observation. A newly monitored
enrolment appears in `baseline.new_enrolments` and receives its own baseline;
its old records do not become new events. A selected-subset check preserves
other enrolment baselines.

Record identity is namespaced by profile, enrolment, record kind, and stable
source identity when available. Mutable content is not identity. Every
committed transition receives a stable event ID and revision; a later change
back to an old value is a new revision. Where source identity is unavailable,
later work must preserve ambiguity and must not collapse equal-looking records.
Source notes and links are untrusted data. They are never commands or agent
instructions.

The planned consumer interface is a bounded, non-destructive batch with a
stable batch/high-water token and deterministic event order. Acknowledgement
advances only the named consumer through that exact batch, is idempotent, and
cannot acknowledge events committed after the batch was read. Delivery is at
least once; external notification deduplication uses event IDs. These consumer
and durable-journal mechanisms are contract requirements, not features of this
intermediate implementation.

Schema-v2 commands remain usable during the transition. A future storage slice
must either migrate version-2 state transactionally while preserving the
original for recovery, or refuse it with `invalid_state` and an actionable
route. It must not fabricate unavailable history or reinterpret legacy
`changes.json` as a durable journal. Legacy `sync --consumer` directories are
separate baselines; later storage must migrate them into one account/profile and
origin-bound journal with independent consumer cursors, or refuse ambiguous
state. It must not keep fetching the portal once per consumer.
