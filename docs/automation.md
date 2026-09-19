# Automation

`ednevnik check` writes schema-versioned JSON to stdout. Errors are JSON on
stderr. It is noninteractive.

```sh
ednevnik check --profile family --student 1234567 --student 2345678
ednevnik status --profile family
```

A profile and portal origin identify one account namespace. Student IDs identify
enrolments, not permanent people. A check of selected enrolments preserves the
baselines of unselected enrolments.

## Outcomes

| Outcome | Exit | Meaning |
| --- | ---: | --- |
| `initial_baseline` | 0 | First complete observation; existing records are not events |
| `complete_with_changes` | 0 | Complete observation with committed events |
| `complete_without_changes` | 0 | Complete observation without changes |
| `incomplete` | 3 | Coverage or timeline continuity was not established; prior baseline is kept |
| `failed` | 1 | Operational failure |

Invalid command arguments exit with status 2. Scripts should use `outcome`,
`reason`, and exit status, not the human-readable `message`.

Each result includes `schema_version`, `check_id`, `profile`,
`requested_enrolments`, `coverage`, timestamps, `baseline`, `changes`, and
`guidance`. `status` is local-only and reports the latest attempt separately
from the last successful check.

## Consumers

Consumers have independent cursors and never contact the portal:

```sh
ednevnik consumer-register --profile family --consumer notifier --start latest
ednevnik consumer-read --profile family --consumer notifier --limit 100
ednevnik consumer-ack --profile family --consumer notifier --token TOKEN
```

Delivery is at least once. Until a token is acknowledged, `consumer-read`
returns the same batch. External effects should be deduplicated by event ID.

The state store retains at most 10,000 events, 32 consumers, 1,024 recent
acknowledgement tokens per consumer, and 1,000 events per read. A limit failure
does not replace the last valid state.

`sync --profile NAME --student ID` is an alias for `check`. The older `sync`,
`changes`, and unprofiled `status` commands use the schema-version 2 snapshot
interface and do not provide timeline catch-up guarantees.
