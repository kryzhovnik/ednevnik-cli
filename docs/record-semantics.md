# Record reconciliation semantics

Schema-v3 checks reconcile normalized records before the atomic state commit.
`Change.RecordKey` is the identity seam. A key is derived from the configured
profile and canonical portal origin, enrolment, record kind, and source
identity. Mutable values, status, and notes are not part of a stable-source
key.

Timeline items use the portal item identity and type. Grade and absence detail
records use a source record identifier when the page supplies one. The current
recognized detail markup can also omit that identifier. In that case, the
fallback group uses the enrolment plus record kind and the source fields that
identify the class or assessment slot: subject, date, period for an absence;
subject identifier, date, and assessment kind for a grade. Value, status, and
note remain mutable.

Fallback matching preserves multiplicity. Equal-looking records are compared
as a multiset rather than deduplicated. A singleton prior and singleton current
record in a fallback group form an update. Several unmatched records
on both sides produce an `*_ambiguous` change with all bounded before and after
states. A cardinality change in an existing fallback group is also ambiguous:
DOM position is not treated as durable identity, even if unchanged content can
be paired. The tool does not guess a pairing. Records first observed together
receive separate occurrence keys, while only singleton 1-to-1 continuity uses
that occurrence identity for later updates. Older schema-v3 baselines whose parser IDs included
mutable content reconcile through the fallback fields, so they do not create a
historical addition flood. Existing retained event IDs are not rewritten.
Once a fallback group becomes ambiguous, its group key is retained with the
baseline. Later singleton corrections stay ambiguous and cannot inherit an old
occurrence lineage. A disappearance does not clear this marker because the
source did not establish deletion; reappearance remains ambiguous. Explicit
rebaselining or future stronger source identity can start a new lineage.
The same marker is created when a previously unambiguous fallback group merely
disappears from a source that cannot prove deletion. A later record at that
anchor is therefore ambiguous rather than a new absence or an old occurrence.
Revision lineage from early schema-v3 events without `RecordKey` is recovered
only for absences, timeline activities, and overview subjects whose retained
immutable fields support it. Old grade events lack the subject identifier,
assessment kind, and multiplicity needed for safe recovery; they stay retained
with their original IDs, while a later safely identified grade starts a new
revision lineage rather than fabricating a match.

Every update carries `before` and `after` record states. A transition event is
ordered by its retained sequence and has a revision scoped to its record key.
Changing A to B and later B to A creates two events and advances the revision.
Committing the same check transition again preserves its event ID, sequence,
and revision; conflicting reuse of that event identity is rejected.

Grade-overview changes have source `grade_overview` and meaning
`displayed_grade_list_changed`. They report only the displayed value list that
was read. Timeline changes have source `timeline`. These signals are not
merged without a shared source identity, and consumers can keep them separate.

Disappearance alone does not prove removal. Timeline records are a bounded
feed, and current absence or selected grade views are not treated as deletion
complete. Reconciliation therefore emits no removal unless the caller
explicitly supplies source-specific deletion-completeness evidence. Incomplete
checks do not invoke successful reconciliation or advance the committed
baseline.

Stable source identities have a bounded last-known state in each enrolment
baseline. It is seeded by the initial observation and is not removed when a
record leaves a non-deletion-complete view. If that identity returns, the tool
emits `*_reappeared` with meaning `observed_reappearance`, the same record key,
and last-known/current context. This continues its retained revision lineage
instead of reporting a new school record. Selected subsets preserve the other
enrolment baselines, and incomplete checks do not replace this history.
