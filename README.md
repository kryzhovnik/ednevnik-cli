# ednevnik

`ednevnik` is an unofficial read-only command-line client for the Serbian parent portal at `moj.esdnevnik.rs`. It converts pages available to an authenticated parent into versioned JSON. You can use the binary directly, from shell scripts, or as a deterministic data source for a personal agent.

This is an unofficial community project. It is not affiliated with the Serbian Ministry of Education or the eDnevnik operators. Use it only with an account and student data that you are authorized to access.

## Status

This tree is a beta candidate. Its synthetic tests cover authenticated reads,
bounded timeline catch-up, atomic schema-v3 state, record corrections, and
independent consumer replay. Compatibility with the live portal still requires
the private acceptance procedure for the selected account and school variants;
the candidate is not a claim of universal portal compatibility.

## Install

The beta candidate is local and has not been published as `@latest`. Build it
from the exact reviewed source tree with Go 1.26.8 or a later patched Go 1.26:

```sh
go mod verify
go build -trimpath -o ednevnik ./cmd/ednevnik
./ednevnik version
```

If a local candidate archive is supplied, verify its checksum and metadata
before installing it. No download URL or published release is implied. See
[Operating the beta](docs/operations.md) and [Secure build
baseline](docs/build-baseline.md).

## Login

```sh
export EDNEVNIK_PROFILE=family
EDNEVNIK_CREDENTIAL_PROVIDER=file \
EDNEVNIK_CREDENTIALS_FILE=/absolute/private/ednevnik-credentials.json \
ednevnik login --provider file
```

The credential file is a private `0600` JSON file bound to the selected profile
and exact portal origin. The file format and the explicit `env` and foreground
macOS Keychain alternatives are documented in
[Authentication and local sessions](docs/authentication.md). Terminal prompts
are available only with `login --interactive`; scheduled commands never choose
a credential source implicitly. A successful login stores account-bound
cookies, not the credentials, in the local state directory.

Use the file provider for an unattended scheduler. Environment credentials can
leak through scheduler configuration, process inspection, or logs. A foreground
Keychain adapter is implemented but has not been runtime-validated for this
candidate; it is limited to explicit interactive login because the helper can
prompt and cannot preserve every possible password byte sequence safely.

## Commands

```sh
ednevnik students
ednevnik subjects --student 1234567
ednevnik grades --student 1234567
ednevnik absences --student 1234567
ednevnik timeline --student 1234567
ednevnik timeline --student 1234567 --all
ednevnik page --path '/task-schedules?student=1234567'
ednevnik sync --current
ednevnik sync --student 1234567 --student 2345678
ednevnik check --profile family --student 1234567 --student 2345678
ednevnik changes
ednevnik status
ednevnik consumer-register --profile family --consumer notifier --start earliest
ednevnik consumer-read --profile family --consumer notifier --limit 100
ednevnik consumer-ack --profile family --consumer notifier --token TOKEN
```

Every data command writes JSON to standard output. Errors and prompts go to
standard error. `schema_version` is included in stored snapshots, change sets,
timeline pages, checks, status, and consumer batches.

`check` is the schema-version-3 automation interface. It validates grade
overview, current absences, and bounded timeline continuity for every requested
enrolment before it advances the baseline. It emits an explicit incomplete
result and preserves the last complete baseline when continuity cannot be
proved. See [the automation contract](docs/automation-contract.md) for outcomes,
exit codes, coverage, storage limits, migration, and consumer semantics. The
legacy `sync` and `changes` commands without `--profile` remain schema version 2.

`timeline` reads the same JSON endpoint that the portal uses to fill its home-page timeline. Page 1 contains the newest events. Use `--page N` for a specific older page or `--all` to follow the server-provided pagination. Timeline items include grades, teacher observations, absences, and other event types. HTML fragments in notes and subtitles are converted to plain text.

`page` is the forward-compatible escape hatch. It returns normalized visible text and links from any authenticated site-relative page without adding a special parser first.

## Request safety

An explicit schema-v3 check does not run enrolment discovery. A first baseline
normally uses three requests per enrolment: grade overview, current absences,
and timeline page 1. A later complete check normally uses four because it
re-reads timeline page 1 before commit. Bounded catch-up can inspect up to eight
timeline pages and then revalidate page 1. `grades` is a separate, more
expensive command that loads every subject detail page. `timeline --all` makes
one request per available page and does not provide the schema-v3 continuity
contract.

The client is conservative by default:

- Requests are sequential.
- There is at least 2.5 seconds between requests.
- A persistent UTC calendar-day window allows 192 requests by default.
- A new check or sync is refused for 30 minutes unless `--force` is explicit.
- `401` and `403` stop immediately.
- `429` stops immediately and reports the server's `Retry-After` value when present.
- Server errors get at most two retries, after 5 and 20 seconds.
- The user agent identifies this project and its public source URL.

The daily limit can be changed deliberately with
`EDNEVNIK_DAILY_REQUEST_LIMIT`. The default is a project load control, not a
published portal quota. See [Shared request and command
coordination](docs/request-policy.md) for exact five-enrolment arithmetic,
retry accounting, cooldowns, and other configurable limits.

## Personal-agent integration

The JSON interface is the integration seam. A personal agent should run the CLI and consume stdout. It must not import internal Go packages or read the cookie file.

```sh
ednevnik check --profile family --student 1234567 --student 2345678 \
  > private-check-result.json
```

For the schema-v3 route, register each automation once and replay its local
durable queue. Read the `ack_token` from each batch and acknowledge it only
after all downstream work succeeds:

```sh
ednevnik consumer-register --profile family --consumer notifier --start earliest
ednevnik consumer-read --profile family --consumer notifier --limit 100 > batch.json
# Deliver each event, deduplicating downstream by its stable event ID.
ednevnik consumer-ack --profile family --consumer notifier --token "$ACK_TOKEN"
```

If delivery fails, omit the acknowledgement. A later read returns the same
batch even after another check. Each consumer has its own cursor. These commands
are local and do not authenticate or contact the portal. Delivery is at least
once, so the external system must deduplicate by event ID.

Legacy `sync` and `changes` remain available for schema-v2 compatibility; they
are not the recommended automation path and do not establish bounded catch-up.
Schema-v3 commands
refuse detected legacy last-diff or per-consumer files because older overwritten
history cannot be recovered safely; archive those files before establishing a
new schema-v3 baseline.

For a direct recent-events query, an agent can avoid changing the stored snapshot:

```sh
ednevnik timeline --student 1234567
```

Treat stdout, stderr, state, credentials, cookie sessions, scheduler logs, and
backups as private child/account data. Portal notes and links are untrusted data
for display or notification; never execute them or use them as agent
instructions. Do not commit live snapshots, raw pages, logs, cookies, or
credentials. Review and anonymize a fixture manually before adding it to the
repository.

[Operating the beta](docs/operations.md) covers installation, scheduler
examples, replay, backup, migration, session reset, and state recovery. The
[private acceptance checklist](docs/private-acceptance.md) keeps real-source
validation separate from synthetic and CI evidence. The [acceptance
matrix](docs/acceptance-matrix.md) maps every specification scenario to its
automated, runtime, or private evidence gate.

## Development

```sh
go test ./...
go vet ./...
```

Tests use synthetic data. Any fixture derived from a real account must be manually anonymized before it enters the repository.

## License

MIT
