# Automation contract

The beta automation interface is `ednevnik check`. It emits schema version 3
JSON on standard output. Diagnostics and structured command errors use standard
error. Existing `sync` without `--profile`, `changes`, and their stored files remain schema version
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

All commands for that namespace share the process lock and request policy
described in [Shared request and command coordination](request-policy.md).
Consumer names do not create independent request budgets or sessions.

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
retry/recovery guidance. Scripts must not parse `message`. General reasons are
`authentication_provider`, `refusal_quota`, `invalid_source`, `invalid_state`,
`storage_limit`, `concurrency`, `cancelled`, `io`, and `invalid_argument`.
Credential selection can return the more specific `credential_missing`,
`credential_malformed`, `credential_insecure`, `credential_unavailable`,
`credential_conflict`, `credential_account_mismatch`, or
`credential_account_unbound`. Exit 2 means the invocation must be corrected.
Exit 1 means an operational failure. `check` is always noninteractive and uses
only the provider explicitly named by `EDNEVNIK_CREDENTIAL_PROVIDER`; it never
falls back to another source. Whole-command cancellation is `cancelled`;
retrying starts a new check and cannot make the cancelled attempt successful.

The live adapter validates grade overview, current absences, and bounded
timeline pages through the real parsers. A valid empty grade overview has a
complete observed grade-table container. A valid empty absence page has either
the complete legacy categories container or the scoped no-data structure with
the expected absence modal target. Truncated, misplaced, or partially matching
empty markers are invalid. When the page supplies a student-class identifier,
it must match the requested enrolment; an otherwise recognized empty page can
have no such identifier. An arbitrary HTML page is not an empty section.
Subject links inside one complete recognized overview may omit the student
query; if that query is present, it must be well-formed and identify only the
requested enrolment. Individual
records need their essential identifiers and fields. Unknown assessment forms,
unknown absence statuses, and inconsistent timeline pagination are
`invalid_source`; they are not silently omitted. Responses larger than 20 MiB
are rejected before parsing, without returning a parsed prefix.

For an existing enrolment, the runner follows sequential timeline pages until
it overlaps the committed recent boundary or reaches a validated source
boundary. It then re-reads page 1 before commit. Page/time exhaustion, a
repeated page, a moving head, or an unproven boundary returns `incomplete` and
leaves the prior successful baseline unchanged. This proves continuity only
through the inspected bounded feed; it is not all-history coverage.

Synthetic fixtures cover the selectors and JSON shapes currently observed by
the parsers, including complete empty `.flex-table` and `.categories-wrap`
containers, the scoped absence no-data structure, and overview links with and
without a valid student query.
They do not prove that every real school, school year, or portal rollout uses
those shapes. The private acceptance pass must compare authenticated real
pages for: a non-empty and genuinely empty grade overview; a non-empty and
genuinely empty absence page; numeric and any non-numeric assessment forms;
student discovery with old and current enrolments; and timeline pagination
metadata. Any new source variant stays unsupported until its structure and
coverage evidence are reviewed and represented by an anonymized fixture.
Legacy `sync --current` also refuses an empty current selection as
`no_current_enrolments` after a structurally valid non-empty family discovery,
and refuses discovery that loses an enrolment marked current in the previous
snapshot. The current-year flag is inferred from the newest parsed school year
and a non-withdrawn class label because no stronger current-enrolment marker is
established by the available fixtures. The private acceptance pass must verify
this inference for same-year transfers and closed enrolments before it is
treated as real-portal compatibility evidence.

`status --profile NAME` is local-only. It never contacts the portal or loads
credentials. It reports `latest_attempt` separately from `last_success`. A
schema-v3 account reports `history: retained_events`; events are read through
the local `consumer-read` command. With no schema-v3 state, history is
`unavailable`. Plain `status` and `status --consumer` preserve their schema-v2
shape during the transition.

The first complete check establishes a bounded recent baseline from the
validated overview, current absences, and inspected timeline pages. Existing
records do not become events. A newly selected enrolment follows the same rule.
For an existing enrolment, the baseline advances only after bounded continuity
and head stability are established. Otherwise the result is `incomplete`.
This is recent-feed continuity, not all-history or historical-correction
coverage.

## Downstream contracts

A baseline ID names one coherently committed observation. A newly monitored
enrolment appears in `baseline.new_enrolments` and receives its own baseline;
its old records do not become new events. A selected-subset check preserves
other enrolment baselines.

Record identity is namespaced by profile, enrolment, record kind, and stable
source identity when available. Mutable content is not identity. Every
committed transition receives a stable event ID and revision; a later change
back to an old value is a new revision. Where source identity is unavailable,
fallback matching preserves multiplicity and emits explicit ambiguity instead
of collapsing equal-looking records. Source notes and links are untrusted data.
They are never commands or agent instructions.

The implemented fallback, ambiguity, source labels, before/after fields, and
deletion evidence rules are documented in
[Record reconciliation semantics](record-semantics.md).

Consumers are explicitly registered with `consumer-register --start earliest`
or `--start latest`. `earliest` starts at the first event still retained;
`latest` ignores the current backlog. Registration never changes an existing
consumer. `consumer-read` returns at most the requested limit (1 through 1000)
in increasing event sequence. Until its token is acknowledged, later reads
return that exact event range and token even when another check commits events.
`consumer-ack` accepts only the token delivered to that named consumer. It
cannot include later events. A repeated acknowledgement remains successful
while the token is retained in the consumer's finite history.

Retrieval is at least once. A process that fails after reading must retry the
same batch. An external notifier must deduplicate effects by stable event ID;
the CLI cannot make an external effect exactly once. Consumers have independent
cursors and all consumer operations are local: they take the shared account
lease, update the same atomic document as checks and baselines, and do not load
credentials, create a portal client, or run an implicit sync.

The document has explicit limits: 16 MiB encoded size, 10,000 retained events,
32 consumers, 100,000 durable source/ambiguity identity entries, 1,024 retained
acknowledgement tokens per consumer, and a maximum batch of 1,000 events. This
beta does not automatically delete journal or identity history. Reaching a
limit refuses the check or consumer mutation without replacing the prior
generation. Recovery requires a private archive and an explicitly chosen
profile reset after unread events are accounted for; there is no silent loss.
See [Operating the beta](operations.md) for the archive-first recovery flow.

Schema-v2 commands remain usable during the transition. Schema-v3 `check` and
profile status deliberately refuse known root, per-consumer, namespaced-v2, or
staged `check-status.json` state. The files are preserved. Archive or move the
legacy files, then establish a new schema-v3 baseline. Legacy `changes.json`
contains only the last diff, and older changes may already have been
overwritten; the tool does not fabricate that unavailable history or reinterpret
the file as a durable journal. Legacy per-consumer snapshots also cannot be
merged safely because their baselines can differ.

Schema-v3 account state is one generation file committed by a synced temporary
file, atomic rename, and directory sync while the shared account lease is held.
It contains per-enrolment baselines, retained ordered transition envelopes,
latest attempt, and last success. Incomplete and failed attempts update only
attempt evidence. Selected subsets do not replace unselected baselines.
Transition envelopes include an event ID, sequence, revision, and producing
check ID. There is no automatic event cleanup. The finite refusal policy above
preserves unread events and the durable identity maps used to detect later
corrections.

`sync --profile NAME --student ID` is an explicit alias for the schema-v3
reliable check and uses the same bounded timeline catch-up and coherent commit.
`sync` without `--profile` retains schema-v2 output and storage compatibility;
it reads only the newest timeline page and must not be used as evidence of
complete catch-up. See [Timeline catch-up and coverage](timeline-catchup.md).
Timeline section coverage carries the inspected page range/count and record
count. Its correction-coverage field is separate from continuity evidence.
