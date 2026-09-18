# Authentication and local sessions

Every network command runs in one explicit profile and exact portal origin.
The profile, origin, durable username binding, cookie session, diary history,
and request policy share one locked state namespace. Reusing a profile with a
different username is refused. Select a new profile for another parent account.

Set `EDNEVNIK_CREDENTIAL_PROVIDER` to exactly one provider. The same provider
is used for initial login and for one expired-session recovery attempt:

- `file`: set `EDNEVNIK_CREDENTIALS_FILE` to an absolute regular file owned by
  the current user with mode `0600`. The JSON fields are `schema_version` (1),
  `account` (the profile), `origin` (the canonical HTTPS origin), `username`,
  and `password`. Password whitespace is preserved. A scheduler or secret
  manager can mount this file without copying it into eDnevnik state.
- `env`: set both `EDNEVNIK_USERNAME` and `EDNEVNIK_PASSWORD` in the command
  environment. Missing or partial input fails; no other provider is tried.
- `keychain`: supported only for an explicit foreground login on macOS. The
  `security` command can require UI and its output cannot represent a password
  ending in a newline without ambiguity. Unattended Keychain selection fails
  before launching the helper. Use `file` or `env` for scheduled jobs. No
  scheduler-context Keychain support is claimed without a synthetic runtime
  test on the target Mac.

`login --save` returns `credential_unavailable`: the `security` CLI cannot
safely receive the verified password without exposing it as an argument, and
saving before portal verification could overwrite a working item. Provision
the exact service/account item manually in Keychain Access, then select it with
`--provider keychain --interactive`. The generic-password account is the portal
username and the service is `ednevnik-cli:<profile>:<canonical-origin>`.

Terminal prompting is disabled unless `login --interactive` is explicit and
stdin is a terminal. Selecting Keychain also requires that foreground mode.
When a provider is selected for any network command, its username is loaded
once and checked against the durable binding before the first portal request,
even if the current cookie session is still valid. Configuring both environment
and file input is a `credential_conflict` error.

Use `ednevnik login --provider file` (or `env`) for a fresh explicit login.
It discards the old cookie session before submitting the selected credentials,
then verifies a protected page before saving the replacement. Authentication
does one credential lookup, one login POST, and at most one replay of the safe
read. CAPTCHA, MFA, eID, and browser-only flows return an actionable failure.

Use `ednevnik session-reset` to remove only local cookies. It preserves the
durable account binding, diary baseline, retained events, and consumer state.
It does not revoke a remote portal session. Legacy cookie files cannot be
migrated because they lack scope and ordering metadata; reset and log in again.

Session snapshots are private mode `0600`, versioned, and bound to the profile
and exact origin. They retain host-only/domain scope, path, absolute expiry,
Secure, HttpOnly, quoted values, and deterministic creation order. Initial
targets and redirect hops must stay on the exact origin, including scheme and
port. Credential-bearing 307/308 redirects are refused.

The loopback test transport is isolated from production state and credential
variables. It requires `EDNEVNIK_TEST_STATE_ROOT`; an explicit state override
uses `EDNEVNIK_TEST_STATE_DIR` and the normal `EDNEVNIK_STATE_DIR` is ignored.
Synthetic authentication uses `EDNEVNIK_TEST_USERNAME` and
`EDNEVNIK_TEST_PASSWORD`, or `EDNEVNIK_TEST_CREDENTIALS_FILE`. It refuses the
normal credential variables so a synthetic endpoint cannot inherit them.
