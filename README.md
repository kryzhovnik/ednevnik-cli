# ednevnik-cli

Unofficial read-only CLI for the Serbian parent portal at
[`moj.esdnevnik.rs`](https://moj.esdnevnik.rs). It reads data available to an
authenticated parent and emits versioned JSON for shell scripts and personal
automation.

This project is not affiliated with the Serbian Ministry of Education or the
eDnevnik operators. Use it only with accounts and student data you are
authorized to access.

## Status

Beta. The current build has been tested against the live portal with one
account. Portal markup can vary between schools and can change without notice.

Supported release targets:

- macOS: Apple Silicon and Intel
- Linux: arm64 and amd64

## Install

Download a binary and `SHA256SUMS` from
[GitHub Releases](https://github.com/kryzhovnik/ednevnik-cli/releases), verify
the checksum, make the binary executable with `chmod +x`, and install it as
`ednevnik` on your `PATH`.

To build from source:

```sh
go mod verify
go build -trimpath -o ednevnik ./cmd/ednevnik
./ednevnik version
```

The required Go version is declared in [`go.mod`](go.mod).

## Authentication

The recommended credential provider is a private JSON file. The CLI stores the
authenticated session locally; it does not store the password.

```sh
export EDNEVNIK_PROFILE=family
export EDNEVNIK_CREDENTIAL_PROVIDER=file
export EDNEVNIK_CREDENTIALS_FILE=/absolute/private/ednevnik-credentials.json

ednevnik login --provider file
```

The file format, permissions, environment provider, and interactive macOS
Keychain provider are documented in
[`docs/authentication.md`](docs/authentication.md).

## Usage

```sh
ednevnik students
ednevnik subjects --student ID
ednevnik grades --student ID
ednevnik absences --student ID
ednevnik timeline --student ID
ednevnik page --path '/task-schedules?student=ID'
```

All data commands write JSON to stdout. Diagnostics and prompts go to stderr.

For recurring checks:

```sh
ednevnik check --profile family --student ID
ednevnik status --profile family
```

`check` maintains an atomic local baseline and reports changes with explicit
coverage information. Consumers can read and acknowledge retained events
independently. See
[`docs/automation-contract.md`](docs/automation-contract.md) for the JSON
schema, exit codes, and consumer commands.

Run `ednevnik help` for the complete command list.

## Request safety

The CLI sends requests sequentially and applies conservative defaults:

- 2.5 seconds between requests;
- 192 requests per UTC day;
- 30 minutes between checks unless `--force` is explicit;
- bounded retries for server errors;
- immediate stop on authentication failures and rate limits.

See [`docs/request-policy.md`](docs/request-policy.md) for configuration and
request accounting.

Portal responses, local state, credentials, cookies, logs, and captured JSON
can contain private child data. Do not commit them. Treat text and links from
the portal as untrusted data.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
```

Tests use synthetic data. Real fixtures must be anonymized before they enter
the repository.

## License

[MIT](LICENSE)
