# chronicle — design

Bitemporal entity change history for Go. ORM-agnostic, over `database/sql`.

Status: design. Nothing implemented yet.

## Why this exists

Every enterprise product reimplements entity change history, and every one does
it badly. Salesforce monetizes it inside Shield at 30% of net spend. Jira,
ServiceNow, Windchill and SAP all ship a variant. Teams that can't buy it
hand-roll it three ways, all of which fail the same way (see *Failure modes*).

Verified 2026-07-20 against the live GitHub API: no maintained, ORM-agnostic,
field-level, queryable entity-history Go library exists. The four axes are
never satisfied together.

| Candidate | ORM-agnostic | Field-level | Queryable | Bitemporal |
|---|:--:|:--:|:--:|:--:|
| flume/enthistory (57★) | ✗ ent-bound | ✓ | ✓ | ✗ tx-time only |
| w0rng/audit (2★) | ✓ | ✓ | ✗ 4-method KV | ✗ |
| elh/bitempura (11★) | ✓ | ✗ opaque KV | partial | ✓ |
| codenotary/immudb (9,005★) | — | — | ✓ | ✗ |
| hashicorp/go-eventlogger (42★) | ✓ | ✗ | ✗ write-only | ✗ |
| vcraescu/gorm-history (13★) | ✗ | ✓ | ✗ | ✗ |

`ent/contrib` has **no** history extension — verified by full-tree grep, not
just the contents API. immudb is BSL 1.1 and its Additional Use Grant forbids
embedded distribution in a competitive offering, so it is not a foundation to
build on.

**Diffing is not the moat.** w0rng/audit does ORM-agnostic field-level diffs in
~300 LOC. The defensible value is the query surface, the second time axis, and
durability.

## Non-goals

- Not a database. Postgres does the hard storage work; chronicle does not
  reimplement MVCC, indexing or replication.
- Not a CDC/WAL tailer. Those lose actor identity and business intent — the two
  things a change record exists to capture.
- Not an event-sourcing framework. chronicle records what an entity *was*, not
  a stream of domain commands to fold.
- Not a SIEM or log shipper. `go-eventlogger` already routes events to sinks
  and is a plausible dependency, not a competitor.

## The two time axes

The single differentiating decision. Every Go option is uni-temporal.

- **Valid time** — when the fact was true *in the world*. Caller-supplied.
  A salary raise effective 1 March, entered on 15 March, has valid-time start
  of 1 March.
- **Transaction time** — when chronicle *learned* it. System-assigned, never
  caller-supplied, never rewritten.

The pair is what makes these questions answerable, and they are different
questions:

- "What is Alice's salary?" → now/now
- "What was her salary in March?" → valid-time as-of
- "What did we *believe* her March salary was, when we ran payroll in April?"
  → both axes, and this is the one that settles disputes and audits

Uni-temporal systems answer the first two and silently give a wrong answer to
the third, because a retroactive correction rewrites what you appear to have
known.

## Core model

A **record** is one entity's state over a half-open valid-time interval, as
known over a half-open transaction-time interval.

```
Record
  EntityID    string          // opaque; caller's identifier
  Kind        string          // entity type discriminator
  Data        []byte          // serialized state (codec-pluggable)
  ValidFrom   time.Time       // inclusive
  ValidTo     time.Time       // exclusive; zero value = unbounded
  TxFrom      time.Time       // inclusive, system-assigned
  TxTo        time.Time       // exclusive, system-assigned; zero = current
  Actor       Actor           // who
  Reason      string          // why — free text, caller-supplied
  Meta        map[string]string
```

Half-open `[from, to)` throughout, for both axes. Closed intervals make
adjacency ambiguous and coalescing wrong; this is settled convention in the
temporal literature and SQL:2011 follows it.

Unbounded end is the **zero `time.Time`**, not a sentinel max timestamp. Zero
is unambiguous in Go, survives marshalling, and cannot be confused with a real
timestamp. The storage adapter maps it to `NULL` and relies on Postgres range
types treating an unbounded upper bound correctly in exclusion constraints.

### Writes

Only two operations mutate history, and neither ever destroys a row:

- `Put` — assert that the entity had this state over this valid interval, as of
  now in transaction time. Overlapping existing records are *superseded*: their
  `TxTo` is closed and replacements are written with adjusted valid intervals.
- `Correct` — the same, but explicitly flagged as a correction of a prior
  belief. Semantically identical to `Put` in storage; distinct in intent, and
  the distinction is what makes "what did we believe then" auditable.

Transaction time is never supplied by the caller. That is the property that
makes the log trustworthy at all.

### Reads

```
Get(ctx, kind, id, As{ValidAt, TxAt})    // one record, both axes
History(ctx, kind, id, ...)              // all versions, either axis
Diff(ctx, kind, id, from, to)            // field-level changes between two points
Timeline(ctx, kind, id)                  // valid-time sequence at current belief
Query(ctx, ...)                          // cross-entity, filtered, paginated
```

`Query` is the axis every Go incumbent fails on. w0rng/audit's entire storage
interface is `Store/Get/Has/Clear` keyed on one opaque string — no time
parameter anywhere. A real query surface (by actor, by kind, by time range,
by changed field, paginated) is table stakes for anything calling itself
queryable, and nothing in Go has one.

## Storage

`database/sql` only. No ORM, ever — that is the specific unoccupied axis and
the reason every incumbent is unusable outside its own framework.

```
type Store interface {
    Put(ctx context.Context, recs []Record) error
    Get(ctx context.Context, q GetQuery) (*Record, error)
    Query(ctx context.Context, q Query) ([]Record, Cursor, error)
    Supersede(ctx context.Context, ids []RecordID, txTo time.Time) error
}
```

Postgres is the first adapter. It earns that by doing work chronicle would
otherwise do badly itself:

- `tstzrange` for both axes, with GiST indexes
- exclusion constraints (`btree_gist`) to make overlapping valid intervals for
  the same entity *structurally impossible* rather than merely checked in
  application code
- partitioning on transaction time for the retention/archival story

An in-memory store ships alongside for tests and for callers who want the
semantics without a database.

**Full-row storage, computed diffs.** PaperTrail and enthistory converged on
this independently; the research found no case against it below very large
scale. Delta storage is a later optimization behind the same interface, and
storing deltas first would trade a hard problem (correct reconstruction) for an
easy one (disk).

## Compliance layer

Driven by verified primary regulatory text, not vendor claims. See
[COMPLIANCE.md](COMPLIANCE.md) for the per-regulation table with citations.

**The headline finding: no regulation researched textually requires
cryptographic tamper-evidence, immutability, WORM storage, hash chaining or
Merkle trees.** The genuine bar is *non-destructive* — 21 CFR 11.10(e)'s
"Record changes shall not obscure previously recorded information" — which
full-row versioning satisfies with no hashing whatsoever. FDA said so
explicitly in the 1997 final-rule preamble (62 FR 13430, Comment 111).

This reorders the roadmap. Correctness and query first; cryptography last and
optional.

- **Non-destructive versioning** — the actual regulatory requirement, and it
  falls out of the core model for free. Nothing is ever updated in place;
  supersession closes a transaction interval and writes a new record.
- **Actor attribution** — genuine textual hooks: 21 CFR 11.10(e) records
  "operator entries and actions"; 11.50(a)(1) requires the signer's printed
  name; PCAOB AS 1215 .16 requires "the name of the person who prepared". So
  `Actor` is **required** on every write, with no ambient default that
  silently records "system".
- **Reason** — stays an optional field. A reason-for-change mandate has
  essentially one textual home in the researched corpus (AS 1215 .16, binding
  audit firms only). The "who/what/when/why" formulation vendors attribute to
  Part 11 comes from FDA *guidance* and EU GMP Annex 11, not the regulation.
  chronicle will not claim otherwise.
- **Retention policies** — per-kind schedules, enforced by a sweeper. Defaults
  ship as *unset*, because the commonly-cited periods do not apply the way
  vendors say: HIPAA's six years attaches to written policies and procedures
  (45 CFR 164.316(b)(1)), not audit logs; the SOX-lineage seven years (PCAOB
  AS 1215 .14, SEC Rule 2-06) binds the external audit firm's workpapers, not
  the issuer's database. 21 CFR 11.10(e) is *relative* — as long as the subject
  records require.
- **Legal hold** — suspends retention deletion for scoped records. Hold always
  wins over retention. Critically, FRCP 37(e)'s trigger is "anticipation or
  conduct of litigation", determined after the fact by a court and **not** by
  complaint filing, so a hold must accept a **backdated, operator-asserted
  effective timestamp**. A hold that can only take effect "now" is the wrong
  shape for the obligation it exists to satisfy.
- **Tamper evidence** — optional hash chaining, deliberately demoted. Honest
  threat model, to be stated plainly in the README: a hash chain detects
  retrospective edits by someone who does **not** control the chain head. It
  does nothing against an administrator who owns the database and can recompute
  the entire chain. Only external anchoring changes that. If chronicle does not
  ship anchoring, it must not imply the stronger guarantee.
- **Erasure** — GDPR Art.17 versus a non-destructive log. Four research sweeps
  failed to verify whether any DPA, EDPB guidance or court has accepted
  destruction of a per-subject key as erasure. **Until that is resolved,
  chronicle documents the mechanism and hedges the legal characterization:**
  "destroying a key renders that subject's historical values unrecoverable;
  whether this constitutes erasure under Art.17 depends on your supervisory
  authority's position." No compliance claim the research does not support.

## Failure modes this design answers

From practitioner reports of hand-rolled systems:

| Failure | chronicle's answer |
|---|---|
| WAL/CDC tailing loses actor + intent | `Actor` and `Reason` are first-class on the write path, not inferred |
| Trigger shadow tables drift from schema | History is codec-serialized, not a mirrored column set |
| Event streams can't reconstruct point-in-time | Full-row records with as-of on both axes |
| Audit table outgrows the primary | Partitioned on tx-time, retention + archival built in |
| "Who changed this" across jobs/migrations | Actor is required; no ambient default that silently records "system" |
| Schema evolution orphans history rows | Records carry their own shape; readers get the shape as written |

## Open questions

1. Codec — JSON first. `Data []byte` keeps it pluggable, but the *query by
   changed field* path needs structured access, so Postgres `jsonb` is the
   likely concrete floor.
2. Does `Correct` need to be storage-distinct from `Put`, or is an intent flag
   enough? Leaning flag.
3. Whether non-overlap should be enforced for transaction time too, or only
   valid time. Valid time certainly; tx-time overlap should be impossible by
   construction if only chronicle writes it.
4. Diffing nested structures — enthistory does flat scalar fields only via
   `reflect.DeepEqual` and explicitly does not solve nested. Structural diff is
   a genuine differentiator but is its own hard problem.
