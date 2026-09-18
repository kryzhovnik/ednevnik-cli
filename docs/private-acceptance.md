# Private acceptance checklist

This checklist is the human gate after the local candidate is built and
verified. Preparing it does not authorize access to an existing account,
credential store, cookie file, or child record. Run it only with an account the
operator explicitly selects and is authorized to use, or with deliberately
redacted captures supplied for this purpose.

Synthetic tests prove deterministic behavior and a five-enrolment workload.
They do not prove current live markup, authentication, scheduler operation,
legal permission, operator approval, or compatibility with every school.

## Select the evidence path

Record one path before beginning:

1. **Operator-run:** the operator runs the commands locally and returns only a
   reviewed pass/fail summary.
2. **Redacted captures:** the operator supplies the smallest reviewed source
   pages and expected normalized JSON needed for the selected cases. Static
   captures do not prove live authentication or scheduler behavior.
3. **Bounded live check:** the operator explicitly names the account/profile,
   permitted enrolments, credential provider, state root, and commands. Existing
   credentials or sessions are not implicit authorization.

Keep portal/source compatibility and scheduler credential behavior as separate
selections. Either one can remain unexecuted while the other is checked.

## Prepare a private workspace

Use the exact candidate binary and record its version, artifact checksum,
source commit, OS, architecture, and Go/build metadata. Create a new private
capture directory and isolated state root; do not reuse or inspect normal state:

```sh
umask 077
private_dir=$(mktemp -d "${TMPDIR:-/tmp}/ednevnik-private-acceptance.XXXXXX")
export EDNEVNIK_STATE_DIR="$private_dir/state"
export EDNEVNIK_PROFILE=private_acceptance
mkdir -p "$private_dir/captures"
```

Do not enable shell tracing. Keep terminal scrollback, command history,
scheduler logs, backups, and crash reports private. Do not copy this directory
into the repository. When the evidence has been summarized and reviewed, move
the directory to encrypted private storage or remove it through the operator's
normal secure-data process.

For live authentication, create a new `0600` credential file bound to profile
`private_acceptance` and the exact canonical origin. Select `file` explicitly.
Do not read an existing Keychain item, environment, session, or credential file
without the operator selecting it for this run.

## Source compatibility cases

Capture stdout to private files and stderr to a separate private log. For each
case, compare the normalized result with the same records visible in the portal
or with the deliberately prepared expected JSON.

- **Enrolment discovery:** run `ednevnik students`. Confirm every selected
  current and old enrolment, school year, class, school, and current/withdrawn
  interpretation. Check a same-year transfer or closed enrolment if the chosen
  account contains one. A five-child synthetic fixture is not evidence for this
  inference.
- **Grade overview:** for a non-empty enrolment, run `subjects --student ID` and
  compare subject identities and displayed grade lists. Run it for a genuinely
  empty overview if one is available. Confirm the recognized empty container is
  accepted rather than treating maintenance or truncated HTML as empty. A
  subject link may omit the student query only inside one complete recognized
  overview; a present query must match the selected enrolment.
- **Grade details:** run `grades --student ID` for the smallest enrolment needed
  to cover numeric and any available non-numeric assessment forms. Record which
  forms were actually observed. The schema-v3 routine check monitors overview
  values; a detail read does not expand routine check coverage.
- **Absences:** run `absences --student ID` for a non-empty case and compare
  source identity, date, subject, period, status, and note. Check a genuinely
  empty absence section if available. A scoped no-data marker is accepted only
  with the recognized absence-page structure and modal target. Record which
  justification statuses were seen.
- **Timeline page 1:** run `timeline --student ID --page 1`. Compare item source
  identity, type, date, title/subtitle/note, URL, current page, next page, and
  last page. Treat all note and link text as untrusted data.
- **Timeline pagination:** if page 1 advertises page 2, capture page 2 explicitly
  and verify page numbers and overlap behavior. Then run a schema-v3 check from
  a prior baseline and confirm coverage reports the inspected range and either
  a proved overlap or an explicit incomplete reason. Do not use an unbounded
  history scrape merely to satisfy this case.
- **Invalid source distinction:** only if deliberately supplied by the operator,
  compare a maintenance page, success-only JSON, truncated container, or
  unsupported record. Confirm it is rejected as `invalid_source`, not committed
  as empty data. Do not trigger a portal failure deliberately.

If the selected evidence has no legitimate empty section, old enrolment,
transfer, non-numeric grade, paginated timeline, or invalid variant, mark that
case **not observed**. Do not infer a pass from absence.

## Check, continuity, and correction cases

Run an initial explicit check for only the selected enrolments:

```sh
ednevnik check --profile private_acceptance \
  --student ID1 --student ID2 \
  >"$private_dir/captures/check-initial.json" \
  2>"$private_dir/captures/check-initial.stderr"
```

Confirm `schema_version: 3`, `initial_baseline`, one coverage entry per requested
enrolment, complete grades/absences/timeline sections, complete continuity, and
no historical event flood. Run a later unchanged check after the minimum
interval and confirm `complete_without_changes`, a new last-success timestamp,
and no consumer cursor movement.

Register two private consumers before an observed change. After a later change,
read one batch from each. Leave the first consumer unacknowledged, run another
check, and confirm its exact token/event range replays. Acknowledge that token
and confirm later events remain. Confirm the second consumer still receives the
full independent backlog. Consumer commands must cause no portal traffic.

For a naturally occurring edit, justification, correction, or value returning
to an older value, confirm the retained event keeps a stable record identity,
increments revision, and includes bounded `before` and `after` context. Confirm
equal-looking distinct records remain distinct or are explicitly ambiguous.
Confirm an item merely leaving the recent timeline is not reported as deleted.
Do not alter school data to manufacture this case. If no such transition occurs
during the acceptance window, mark correction behavior **not observed**.

Add one newly selected enrolment or run a selected subset if the account allows
it. Confirm the new enrolment gets an explicit baseline without historical
events and unselected baselines are preserved. If the case is unavailable, mark
it **not observed**.

## Offline comparison tool

Create expected JSON manually from the selected portal view or reviewed redacted
capture. Keep both expected and actual files private. The repository tool
canonicalizes only the supplied projection, compares it without network access,
prints only `match` or `different`, and removes its private temporary files:

```sh
scripts/compare-private-json.sh \
  "$private_dir/expected/students.json" \
  "$private_dir/captures/students.json" \
  'map({id,name,school,class,school_year,current}) | sort_by(.id)'

scripts/compare-private-json.sh \
  "$private_dir/expected/subjects.json" \
  "$private_dir/captures/subjects.json" \
  'map({id,name,displayed_grades}) | sort_by(.id)'

scripts/compare-private-json.sh \
  "$private_dir/expected/absences.json" \
  "$private_dir/captures/absences.json" \
  'map({id,source_id,date,subject,period,status,note}) | sort_by(.id)'

scripts/compare-private-json.sh \
  "$private_dir/expected/timeline-page-1.json" \
  "$private_dir/captures/timeline-page-1.json" \
  '{current_page,next_page,last_page,items:(.items | map({id,portal_id,date,type,title,subtitle,note,url}) | sort_by(.id))}'
```

A `different` result intentionally prints no private diff. Review the two
private projections locally with a trusted JSON tool. Never paste the diff into
an issue or commit it. The comparison proves equality only for the chosen jq
projection and selected capture.

Use local structural checks without displaying content:

```sh
jq -e '.schema_version == 3 and (.coverage | length > 0)' \
  "$private_dir/captures/check-initial.json" >/dev/null
jq -e '.events | all(.id != "" and .revision >= 1 and .sequence >= 1)' \
  "$private_dir/captures/consumer-batch.json" >/dev/null
```

## Scheduler authentication gate

Choose one actual scheduler and host separately from source compatibility:

- macOS launchd with the file provider;
- Linux systemd user timer or cron with the file provider; or
- another explicitly named scheduler environment.

Record the scheduler, OS/architecture, noninteractive stdin, minimal environment,
credential-file ownership/mode, command timeout, and private log paths. Verify
fresh login, persisted-session reuse, forced synthetic expiry or naturally
expired session recovery, cancellation, process locking, restart, consumer
replay, and acknowledgement. Do not claim Keychain scheduling unless the exact
foreground/prompt constraints and selected scheduler context were exercised.
A manually reproduced minimal environment is useful evidence but is not actual
launchd/cron/systemd execution.

## Evidence record and completion

Record only a reviewed, non-identifying summary outside the private workspace:

| Field | Required value |
| --- | --- |
| Candidate | version, source commit, artifact checksum |
| Source evidence | live authenticated, redacted capture, or operator-run |
| Scope | number of children/enrolments, school/page variant count, school-year patterns; no names or IDs |
| Authentication | selected provider and whether expiry recovery was exercised |
| Scheduler | exact scheduler/OS/architecture, or `not executed` |
| Cases | `passed`, `failed`, or `not observed` for every checklist item |
| Retained private data | location/owner or confirmed operator removal; never a repository path |
| Remaining gates | exact missing source variants, scheduler evidence, defects, and policy/legal uncertainty |

Any mismatch creates a focused defect and repeats only the affected checks plus
dependent scenarios after the fix. Private acceptance remains open while a
required source or selected scheduler case is unexecuted. One account or school
variant never establishes universal portal compatibility. Technical success
does not resolve the unverified portal automation policy or authorize
publication.
