# ednevnik-cli

Unofficial read-only CLI for the Serbian parent portal
[`moj.esdnevnik.rs`](https://moj.esdnevnik.rs). It emits JSON for shell scripts
and personal automation.

This project is not affiliated with the Serbian Ministry of Education or the
eDnevnik operators. Use it only with accounts and student data you are
authorized to access. Portal markup can change without notice.

## Install

Download a binary and `SHA256SUMS` from
[GitHub Releases](https://github.com/kryzhovnik/ednevnik-cli/releases), verify
the checksum, run `chmod +x` on the binary, and install it as `ednevnik` on
your `PATH`.

To build from source:

```sh
go mod verify
go build -trimpath -o ednevnik ./cmd/ednevnik
```

## Authentication

The recommended credential provider is a private JSON file:

```json
{
  "schema_version": 1,
  "account": "family",
  "origin": "https://moj.esdnevnik.rs",
  "username": "name@example.com",
  "password": "..."
}
```

```sh
chmod 600 /absolute/private/ednevnik-credentials.json
export EDNEVNIK_PROFILE=family
export EDNEVNIK_CREDENTIAL_PROVIDER=file
export EDNEVNIK_CREDENTIALS_FILE=/absolute/private/ednevnik-credentials.json
ednevnik login --provider file
```

For environment credentials, use provider `env` with `EDNEVNIK_USERNAME` and
`EDNEVNIK_PASSWORD`. Interactive login is available with
`ednevnik login --interactive`. On macOS, an existing Keychain item can be read
with `--provider keychain --interactive`.

The CLI stores cookies and state in the user config directory with private
permissions. `ednevnik session-reset` removes only the local cookie session.

## Usage

```sh
ednevnik students
ednevnik subjects --student ID
ednevnik grades --student ID
ednevnik absences --student ID
ednevnik timeline --student ID
ednevnik page --path '/task-schedules?student=ID'
```

All data commands write JSON to stdout. Diagnostics and errors go to stderr.
Run `ednevnik help` for the full command list.

For recurring checks:

```sh
ednevnik check --profile family --student ID
ednevnik status --profile family
```

`check` keeps an atomic local baseline and reports changes. Local consumers can
read and acknowledge retained events independently. The JSON outcomes, exit
codes, and consumer commands are in [`docs/automation.md`](docs/automation.md).

## Network behavior

The CLI serializes commands for the same profile and portal origin. Its default
policy allows 200 requests per UTC day with 2.5 seconds between requests. It
does not retry server errors. It honors a valid server `Retry-After` value.

Optional local controls:

| Variable | Default | Meaning |
| --- | --- | --- |
| `EDNEVNIK_REQUEST_INTERVAL` | `2.5s` | Minimum delay between HTTP requests; `0` disables it |
| `EDNEVNIK_DAILY_REQUEST_LIMIT` | `200` | UTC-day request limit; `0` disables it |
| `EDNEVNIK_COMMAND_TIMEOUT` | `5m` | Whole-command timeout |
| `EDNEVNIK_COORDINATION_WAIT` | `30s` | Wait for another command using the same profile |

Each redirect is a separate HTTP request. Login commonly uses four requests.
`check` fetches grade overview, absences, and timeline for each enrolment; grade
details are fetched separately only by `grades`. Timeline catch-up is bounded
to prevent an unbounded run.

Portal responses and local state contain private child data. Do not commit
them.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
```

## License

[MIT](LICENSE)
