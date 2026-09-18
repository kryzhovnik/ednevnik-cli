# Operating the beta

This guide covers the local schema-v3 beta. The candidate is not published.
Use an exact reviewed source tree or a locally supplied candidate archive whose
checksum and build metadata match that tree.

## Install and configure

Build from source with Go 1.26.8 or a later patched Go 1.26 release:

```sh
go mod verify
go build -trimpath -o ./ednevnik ./cmd/ednevnik
./ednevnik version
```

Keep the binary, state, captured JSON, scheduler logs, and backups in
user-private locations. The default state root is the operating-system user
configuration directory under `ednevnik`; `EDNEVNIK_STATE_DIR` selects an
absolute operator-managed root. A profile contains lowercase letters, digits,
and underscores. The profile and canonical portal origin form the account
namespace.

For scheduled use, create a regular credential file owned by the scheduler user
with mode `0600`. Edit it without putting the password in command history:

```json
{
  "schema_version": 1,
  "account": "family",
  "origin": "https://moj.esdnevnik.rs",
  "username": "parent-login",
  "password": "exact password bytes represented as JSON"
}
```

Password whitespace is preserved. The account and origin must match the
command exactly. Select this provider explicitly:

```sh
export EDNEVNIK_PROFILE=family
export EDNEVNIK_CREDENTIAL_PROVIDER=file
export EDNEVNIK_CREDENTIALS_FILE=/absolute/private/ednevnik-credentials.json
ednevnik login --provider file
```

The `env` provider requires both `EDNEVNIK_USERNAME` and
`EDNEVNIK_PASSWORD`; it is easier to expose in scheduler configuration and
diagnostics. A macOS `keychain` adapter is implemented for explicit foreground
`login --interactive`, but it has not been runtime-validated for this candidate.
Unattended Keychain access is not supported.

## Checks and consumers

Pass every intended enrolment explicitly. A student ID is an enrolment, not a
permanent child identity:

```sh
ednevnik check --profile family \
  --student 1234567 \
  --student 2345678
```

Exit 0 is a complete result, exit 3 is an incomplete JSON result, exit 2 is a
usage error, and exit 1 is an operational failure. Always inspect `outcome`,
coverage, and `last_success`; HTTP success alone is not coverage evidence.

Register each downstream integration once. `earliest` includes every retained
event; `latest` starts after the current backlog:

```sh
ednevnik consumer-register --profile family --consumer notifier --start earliest
ednevnik consumer-read --profile family --consumer notifier --limit 100 \
  > private-batch.json
```

Deliver every event in the returned batch and deduplicate external effects by
the stable event `id`. Only after every effect succeeds, acknowledge the exact
batch token:

```sh
ack_token=$(jq -r '.ack_token' private-batch.json)
if [ "$ack_token" != "null" ] && [ -n "$ack_token" ]; then
  ednevnik consumer-ack --profile family --consumer notifier \
    --token "$ack_token"
fi
```

If delivery fails, do not acknowledge. The next read returns the same range and
token even if another check has committed more events. Delivery is at least
once; the CLI cannot make an external notification exactly once. Consumer
commands are local and make no portal requests.

## Scheduler examples

Put the account settings and enrolment list in a private wrapper. The wrapper
below retains complete and incomplete JSON without sending it to scheduler
stdout:

```sh
#!/bin/sh
set -eu
umask 077

export EDNEVNIK_PROFILE=family
export EDNEVNIK_CREDENTIAL_PROVIDER=file
export EDNEVNIK_CREDENTIALS_FILE=/absolute/private/ednevnik-credentials.json
result_dir=/absolute/private/ednevnik-results
mkdir -p "$result_dir"
tmp=$(mktemp "$result_dir/check.XXXXXX")
trap 'rm -f "$tmp"' EXIT HUP INT TERM

run_status=0
/absolute/bin/ednevnik check --profile family \
  --student 1234567 --student 2345678 >"$tmp" || run_status=$?
case "$run_status" in
  0|3) mv "$tmp" "$result_dir/latest-check.json" ;;
esac
exit "$run_status"
```

For launchd, install a user agent whose `ProgramArguments` contains only the
absolute wrapper path. Keep `StandardOutPath` at `/dev/null`, and send
`StandardErrorPath` to a private log directory. Do not put credentials or
student IDs directly in the plist. For the documented five-enrolment budget,
use `StartInterval` `21600` (four checks per day).
`RunAtLoad` is optional; account for it in the same daily budget.

For a Linux systemd user timer, use a oneshot service with the absolute wrapper
as `ExecStart`, `UMask=0077`, and `NoNewPrivileges=true`. A corresponding timer
can use `OnBootSec=5m`, `OnUnitActiveSec=6h`, and `Persistent=true`. Treat the
user journal as private because diagnostics can contain enrolment identifiers
and local paths. These are configuration examples. They are not evidence that
launchd or systemd executed the candidate on the operator's selected host.

The defaults are 192 actual requests per UTC calendar day, 2.5 seconds between
requests, 30 minutes between checks, 30 seconds for lock acquisition, five
minutes per command, and 30 seconds per HTTP operation. See
[Shared request and command coordination](request-policy.md) before changing
them. The 30-minute value is a minimum interval, not a recommended schedule;
more frequent runs require explicit request-budget arithmetic.

## Backup, migration, and recovery

Stop the scheduler before copying or moving state. Back up the whole state root
as one private unit; it includes child records, cookie sessions, account
binding, retained events, consumer cursors, and request-policy timestamps. Use
encrypted storage or permissions equivalent to the live state. Do not copy
state into repository artifacts or general-purpose logs.

Schema-v2 `latest.json`, `previous.json`, `changes.json`, `checks.jsonl`, and
per-consumer directories cannot be converted into a trustworthy retained
journal. Schema-v3 commands refuse them and preserve them. Archive the complete
legacy set, then establish a new schema-v3 baseline. The new baseline does not
fabricate overwritten legacy events.

Use `ednevnik session-reset` for an invalid or obsolete cookie snapshot. It
removes only local cookies and preserves the account binding, diary baseline,
retained events, and consumer state. It does not revoke a remote session. The
next network command can authenticate once through the explicitly selected
provider.

For `invalid_state`, storage-limit, or uncertain-output recovery:

1. Stop scheduled and manual commands for the profile.
2. Run `ednevnik status --profile family` locally and retain its structured
   result or error. It does not load credentials or contact the portal.
3. Archive the complete account namespace and the relevant scheduler logs.
4. For an uncertain output, compare the reported `check_id` with
   `latest_attempt` and `last_success` before retrying. A commit may have
   succeeded even when stdout or the final directory sync failed.
5. For corrupt or unsupported state, keep the original bytes. Restore a known
   coherent backup or select a new profile. To reset the same profile, move its
   complete account namespace to a private archive first, account for every
   unread consumer event, and then create a fresh login and baseline. A new or
   reset namespace does not cancel the prior request budget or server cooldown:
   record its retry/window time before the move and wait until it expires before
   making another portal request.

There is no automatic event cleanup. The hard limits are a 16 MiB document,
10,000 retained events, 32 consumers, 100,000 durable identity entries, 1,024
acknowledged tokens per consumer, and 1,000 events per batch. Reaching a limit
refuses the mutation without replacing the last coherent generation.

## Privacy and security boundaries

All CLI JSON can contain child records. Standard error can contain enrolment
identifiers and local paths. Cookie sessions and request-policy files are also
private. Keep shell tracing disabled, use `umask 077`, rotate or bound private
logs, and apply the same controls to backups and crash reports.

Portal notes, labels, and links are untrusted input. Render them as data; do not
execute URLs, shell fragments, or instructions found in them. The CLI sends no
telemetry and performs no notification or LLM call. Any downstream service is a
separate disclosure decision owned by the operator.

Never add raw authenticated pages or live snapshots to fixtures. If a source
variant needs a regression fixture, make a private copy, remove credentials and
stable identifiers, replace names and free text, review the result manually,
and only then add the smallest structure needed by the parser test.
