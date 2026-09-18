# ednevnik

`ednevnik` is an unofficial read-only command-line client for the Serbian parent portal at `moj.esdnevnik.rs`. It converts pages available to an authenticated parent into versioned JSON. You can use the binary directly, from shell scripts, or as a deterministic data source for a personal agent.

This is an unofficial community project. It is not affiliated with the Serbian Ministry of Education or the eDnevnik operators. Use it only with an account and student data that you are authorized to access.

## Status

The project is an early working implementation. Authentication, student discovery, grade extraction, absence extraction, timeline activity extraction, snapshots, and deterministic change detection are present. The parsers must still be verified against more schools and page variants before a stable release.

## Install

Go 1.26 or newer is required while installing from source.

```sh
go install github.com/kryzhovnik/ednevnik/cmd/ednevnik@latest
```

For local development:

```sh
go build -o ednevnik ./cmd/ednevnik
```

## Login

```sh
ednevnik login
```

The command prompts for the username and reads the password without terminal echo. It does not save the credentials. It saves only the resulting cookies under the operating-system user configuration directory. The directory has mode `0700`; files have mode `0600`.

For unattended use, credentials can be supplied through `EDNEVNIK_USERNAME` and `EDNEVNIK_PASSWORD`. Environment variables can leak through process inspection or automation logs. Prefer an operating-system secret store that injects them only for the login process.

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
```

Every data command writes JSON to standard output. Errors and prompts go to standard error. `schema_version` is included in stored snapshots, change sets, and timeline pages.

`check` is the staged schema-version-3 automation interface. The current live
adapter validates the grade overview, current absences, and newest timeline page
but reports incomplete continuity until bounded timeline catch-up is implemented.
It does not claim or commit a complete baseline. See
[the automation contract](docs/automation-contract.md) for outcomes, exit codes,
coverage, migration, and downstream consumer semantics. The existing `sync` and
`changes` commands remain schema version 2 during this transition.

`timeline` reads the same JSON endpoint that the portal uses to fill its home-page timeline. Page 1 contains the newest events. Use `--page N` for a specific older page or `--all` to follow the server-provided pagination. Timeline items include grades, teacher observations, absences, and other event types. HTML fragments in notes and subtitles are converted to plain text.

`page` is the forward-compatible escape hatch. It returns normalized visible text and links from any authenticated site-relative page without adding a special parser first.

## Request safety

`sync --current` discovers every current enrolment and needs three requests per enrolment: one grade overview, one absence page, and the newest timeline page. `grades` is a separate, more expensive command that loads every subject detail page. `timeline --all` makes one request per available timeline page; prefer the default first page for routine checks.

The client is conservative by default:

- Requests are sequential.
- There is at least 2.5 seconds between requests.
- A persistent daily limit allows 100 requests by default.
- A new sync is refused for 30 minutes unless `--force` is explicit.
- `401` and `403` stop immediately.
- `429` stops immediately and reports the server's `Retry-After` value when present.
- Server errors get at most two retries, after 5 and 20 seconds.
- The user agent identifies this project and its public source URL.

The daily limit can be changed deliberately with `EDNEVNIK_DAILY_REQUEST_LIMIT`. Do not raise it without a clear need.

## Personal-agent integration

The JSON interface is the integration seam. A personal agent should run the CLI and consume stdout. It must not import internal Go packages or read the cookie file.

```sh
ednevnik sync --current > ednevnik_snapshot.json

ednevnik changes > ednevnik_changes.json
```

The agent can summarize `ednevnik_changes.json` with an LLM when useful. New timeline records appear as `activity_added` changes. Fetching, parsing, caching, and comparison are deterministic and do not use LLM tokens.

For a direct recent-events query, an agent can avoid changing the stored snapshot:

```sh
ednevnik timeline --student 1234567
```

Treat all output as private child data. Do not commit snapshots, fixtures copied from a real account, cookie files, or credentials.

## Development

```sh
go test ./...
go vet ./...
```

Tests use synthetic data. Any fixture derived from a real account must be manually anonymized before it enters the repository.

## License

MIT
