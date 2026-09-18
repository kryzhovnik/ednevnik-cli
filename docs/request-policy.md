# Shared request and command coordination

Every command that can read or change account state uses one namespace:
the validated profile name plus the canonical portal origin. A consumer name is
never an account namespace. The namespace maps to
`coordination/<profile>-<origin-hash>/` below `EDNEVNIK_STATE_DIR`. It contains
the account lock, request-policy state, session file, check status, and
namespaced schema-v2 state. The hash prevents an
origin string from becoming a path or leaking into file names.

Older schema-v2 files at the unbound state root or its consumer directories are
preserved and refused with a migration error. They cannot be opened under a
new profile lock because two profile locks would then protect the same files.
Plain local schema-v2 `status` and `changes` use a fixed local recovery
namespace and never infer a portal account. Schema-v3 `status --profile`
requires the explicit profile and configured origin.

`coordination.Coordinator.Acquire(ctx)` obtains one command-scoped `Lease`.
The lease is the ownership interface for later store, authentication, and
consumer work. A command acquires it before reading session or account state,
passes it to those modules, and releases it after the last state operation.
Modules must not acquire another account lease while one is held. This single
owner and no-nesting rule is the lock order. It avoids recursive lock deadlocks
and a competing store lock. The operating system releases the advisory lock if
the owner exits or is killed. Acquisition polls with context cancellation and
has a 30-second default maximum wait.

The HTTP transport calls `Lease.BeforeRequest` immediately before every actual
`RoundTrip`. This includes login GET/POST requests, redirects, and each eligible
read retry. The counter is durably incremented before transport, so an uncertain
network outcome remains counted. A request refused locally before `RoundTrip`
is not counted. The command lease also prevents another process from sending
concurrent traffic for the namespace. A 429 or 503 response persists its
`Retry-After`; a missing or invalid value uses six hours. Later commands refuse
locally until the cooldown expires. `--force` does not bypass this refusal.

The defaults are project load controls, not a portal-published quota or operator
approval:

- 192 actual requests per UTC 24-hour window;
- 2.5 seconds between actual requests;
- 30 minutes between checks;
- 30 seconds to acquire the account lease;
- 5 minutes for the whole command and 30 seconds per HTTP operation.

The budget supports a documented five-enrolment workload. One nominal check is
one discovery plus three requests per enrolment, or 16 requests. Four scheduled
checks use 64. A login with a redirect uses up to three. A bounded catch-up may
use eight extra timeline pages per enrolment, or 40. Two retry attempts for each
of the 16 nominal reads add 32 in an adverse check. This totals 139 and leaves
53 requests for command restarts and changed page shapes. At the default pace,
the nominal plus 40-page catch-up takes about 140 seconds before response time,
within the five-minute whole-command bound. Catch-up must still stop incomplete
when its own page or elapsed-time bound is reached.

`EDNEVNIK_DAILY_REQUEST_LIMIT`, `EDNEVNIK_REQUEST_INTERVAL`,
`EDNEVNIK_MIN_CHECK_INTERVAL`, `EDNEVNIK_COORDINATION_WAIT`, and
`EDNEVNIK_COMMAND_TIMEOUT` configure these values. Counts reset only after the
next fixed UTC window boundary. Moving the clock backwards does not grant a new
budget. `--force` has one narrow effect: it bypasses the local minimum check
interval. It never bypasses budget, pacing, server cooldown, validation, or
command cancellation.

If the clock moves backwards, the policy keeps the current budget window and
applies at most one fresh pacing interval instead of waiting until the old wall
clock value returns. A future check timestamp is also treated as inside the
minimum interval and requires an explicit `--force`; it never grants an early
check.

Cancellation covers lock waits, pacing waits, retry delays, and HTTP requests.
Refusals expose a retry time. Local quota or server cooldown maps to
`refusal_quota`; lock timeout maps to `concurrency`; cancellation or a deadline
maps to `cancelled`; authentication failures remain distinct. Existing committed
diary state stays usable because a refused or cancelled request does not replace
it.

Persisted policy input is validated before use. Negative counters and
incoherent budget-window or request timestamps return `invalid_state`; the file
is preserved for explicit repair. A configured budget reduction can leave a
valid count above the new limit and is treated as ordinary quota exhaustion.
