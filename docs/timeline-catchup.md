# Timeline catch-up and coverage

The reliable schema-v3 path is `check --profile NAME --student ID`, also
available as `sync --profile NAME --student ID`. The explicit `--profile`
selects the account/origin-bound check state. `sync` without `--profile` keeps
the schema-v2 snapshot/output contract for compatibility and does not claim
catch-up coverage.

For a new enrolment, the check normally reads and validates only the newest
timeline page. If that page is empty but advertises an older page, the check
continues within the same bounds to avoid committing a false empty boundary.
It commits all inspected records as a recent baseline and emits no historical
events. This policy bounds initial traffic and avoids presenting existing
school history as new activity. It does not establish all-history coverage.

For an existing enrolment, the check follows validated sequential pagination
until a page contains an identity from the last committed newest-page boundary.
If the committed boundary was empty, only a validated end of the current feed
establishes continuity; an empty nonfinal page is traversed. IDs and displayed dates are never treated
as monotonic. Items repeated on overlapping pages are retained once. Only the
items before the first committed anchor on the overlap page can be new events;
an unknown suffix is inspected for corrections but is treated as older history.

Catch-up inspects at most eight sequential pages and takes at most two minutes
per enrolment. A successful existing-enrolment check then makes one additional
request to re-read page 1 before commit. This means at most nine timeline
requests per enrolment: the normal newest-page read, up to seven older pages,
and the head revalidation. The re-read must have the same pagination extent and
content. Changed pagination, a shifted head, or one identity carrying
conflicting content makes coverage incomplete. This is observable stability
evidence, not an atomic snapshot of the remote portal; a change outside the
inspected pages can still happen without being visible. The two-minute context
also bounds an in-flight page request and head revalidation.

An exhausted page/time bound, a missing overlap at the source boundary, or a
changing source returns `incomplete` with exit status 3. Request-budget refusal,
server cooldown, cancellation, and invalid source data return their existing
structured errors. None of these outcomes advances the committed baseline or
consumes retained events. A later check starts again from the last complete
boundary, so tentative pages cannot cause skipped activity.

Coverage reports the distinction directly:

- `recent_baseline_page` with `recent_baseline_established` is the bounded
  initial policy;
- `caught_up_pages` with `timeline_overlap_established` proves new-event
  continuity to the committed boundary;
- `caught_up_to_source_boundary` records the special empty-boundary proof;
- `bounded_catch_up` with an incomplete reason records inspected but
  uncommitted work.

The timeline section also reports `first_page`, `last_page`, `page_count`, and
`records_inspected`. `correction_coverage` is
`inspected_timeline_pages_only`, separate from continuity: continuity proves
that new activity reaches the committed boundary, while correction coverage
states only where edits were compared.

Corrections are detected only for records in the inspected validated pages.
An item leaving the recent feed is window eviction, not deletion evidence.
Edits to old items outside inspected coverage cannot be guaranteed. If the
portal can no longer expose a committed boundary, retry first; then archive the
state and establish an explicit new baseline only after accepting the gap.
