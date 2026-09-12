# ednevnik

`ednevnik` is an unofficial read-only command-line client for the Serbian parent portal at `moj.esdnevnik.rs`. It converts pages available to an authenticated parent into versioned JSON. You can use the binary directly, from shell scripts, or as a deterministic data source for a personal agent.

This is an unofficial community project. It is not affiliated with the Serbian Ministry of Education or the eDnevnik operators. Use it only with an account and student data that you are authorized to access.

## Status

The project is an early working implementation. Authentication, generic page extraction, student discovery, grade extraction, absence extraction, snapshots, and deterministic change detection are present. The parsers must still be verified against more real page variants before a stable release.

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
ednevnik page --path '/task-schedules?student=1234567'
ednevnik sync --current
ednevnik sync --student 1234567 --student 2345678
ednevnik changes
ednevnik status
```

Every data command writes JSON to standard output. Errors and prompts go to standard error. `schema_version` is included in stored snapshots and change sets.

`page` is the forward-compatible escape hatch. It returns normalized visible text and links from any authenticated site-relative page without adding a special parser first.

## Request safety

`sync --current` discovers every current enrolment and needs two requests per enrolment: one grade overview and one absence page. `grades` is a separate, more expensive command that loads every subject detail page.

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

The agent can summarize `ednevnik_changes.json` with an LLM when useful. Fetching, parsing, caching, and comparison are deterministic and do not use LLM tokens.

Treat all output as private child data. Do not commit snapshots, fixtures copied from a real account, cookie files, or credentials.

## Development

```sh
go test ./...
go vet ./...
```

Tests use synthetic data. Any fixture derived from a real account must be manually anonymized before it enters the repository.

## License

MIT
