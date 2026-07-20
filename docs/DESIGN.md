# chronicle — design

Bitemporal entity change history for Go. ORM-agnostic, over `database/sql`.

Status: phases 1–5 implemented — core model, in-memory store, Postgres
adapter, conformance suite, the compliance layer (retention, legal hold,
tamper evidence, crypto-shredding), `chronicled`, the standalone REST
service, and field history, the single-field audit trail. Corrections found
while implementing are recorded inline, marked **Correction**, rather than
silently edited away.

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
Timeline(ctx, kind, id, as)              // valid-time sequence at one belief instant
FieldHistory(ctx, kind, id, path, as)    // one field's changes over transaction time (phase 5)
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
    Apply(ctx context.Context, req ApplyRequest) (time.Time, error) // plan + apply, atomically
    Get(ctx context.Context, q GetQuery) (*Record, error)
    Query(ctx context.Context, q Query) ([]Record, Cursor, error)
}
```

**Correction, found during phase 1.** An earlier version of this design had
separate `Put` and `Supersede` methods. That interface *cannot express the
library's headline invariant*: a write that supersedes three records and
inserts four must not be observable half-applied, and with two independent
calls there is no shared transaction in which to make that true. A reader
landing between them sees either a gap or an overlap in valid time.

`Apply` takes the supersessions and the insertions together, so atomicity is
expressible rather than hoped for. SQL implementations must run it in one
transaction. Phase 1 shipped this as an optional `Atomic` extension to keep the
original four methods; **phase 2 promoted it into `Store` and deleted the
non-atomic fallback**, because that path is a footgun that only looks correct
in single-threaded tests.

**Correction, found during phase 2: `Apply` must return the transaction
instant.** Requirement 3 below asks for database-assigned transaction time, and
an `Apply` returning only `error` cannot deliver it. The log builds every
record with its own proposed `TxFrom` and reports that instant in `Result.TxAt`;
if the store silently substitutes its own, the log hands the caller a timestamp
that is not in the database and resolves later reads of "now" against a clock
that is behind the log's newest record. The instant has to come back out.
`Write.TxAt` [now `ApplyRequest.TxAt`] is consequently a *proposal*, and the
store has the last word.

Postgres is the first adapter. It earns that by doing work chronicle would
otherwise do badly itself:

- `tstzrange` for both axes, with GiST indexes
- exclusion constraints (`btree_gist`) to make overlapping valid intervals for
  the same entity *structurally impossible* rather than merely checked in
  application code — but see the deferrability requirement below
- ~~partitioning on transaction time for the retention/archival story~~

**Correction, found during phase 2: partitioning on transaction time is
incompatible with the exclusion constraint, and the constraint wins.** Postgres
requires every unique or exclusion constraint on a partitioned table to include
the partition key with equality, and `tx_from WITH =` is meaningless for a
non-overlap constraint — two current records for one entity would be free to
overlap as long as they landed in different partitions. Verified against
PostgreSQL 17.10:

```
ERROR:  unique constraint on partitioned table must include all partitioning columns
DETAIL: EXCLUDE constraint on table "p" lacks column "tx_from" which is part of
        the partition key.
```

These are the two headline storage claims and they cannot both be true of one
table. Phase 3 has to pick a different mechanism: partition an *archive* table
rather than the live one, or accept per-partition constraints and enforce
cross-partition non-overlap some other way. Retention was already phase 3; this
just means it is a harder phase 3 than the design assumed.

Three requirements the adapter must satisfy. All three were found in phase 1
review, and all three are correctness issues rather than tuning choices:

1. **The exclusion constraint must be `DEFERRABLE INITIALLY DEFERRED`.**
   Constraints are checked per statement, and a single legitimate `Apply`
   passes through an intermediate state where the superseded record is not yet
   closed and its replacement is already inserted. A non-deferred constraint
   rejects ordinary correct writes.

   **Refinement, found during phase 2.** The premise is conditional on
   statement order, and the order is the adapter's to choose. Closing the
   superseded records *before* inserting their replacements never passes
   through an overlapping state at all, and the shipped adapter does exactly
   that — so a per-statement check would accept every write chronicle makes.
   The requirement is kept, because it costs nothing and it is the only thing
   that keeps the constraint correct under *any* statement order, including a
   future one that batches several writes into one transaction. But the
   justification as stated is not why the shipped code needs it, and a test
   that drove `Apply` and watched it succeed would have proved nothing. The
   deferral is therefore tested through raw SQL in the insert-first order, both
   with the constraint deferred and with it made immediate.
2. **The read-modify-write needs real isolation.** chronicle scans the
   overlapping records *before* computing the split. Two concurrent writers to
   one entity can both observe the same pre-state and each split it, producing
   two current records covering the same instant. In-process mutexes do not
   survive a second process.

   **Correction, found during phase 2.** This requirement originally said the
   adapter must use `SERIALIZABLE` or `SELECT ... FOR UPDATE`. Neither, on its
   own, does what the requirement asks, and `SERIALIZABLE` in particular does
   nothing at all here: the scan happens in a *different transaction* from the
   `Apply` that acts on it, so there is no read dependency inside the write's
   transaction for SSI to track, and both writers commit happily. Row locks
   have the same hole — the rows are read before the locking transaction
   begins, so the lock is taken after the decision it was meant to guard.

   The hazard is not a weak isolation level; it is a read-modify-write split
   across two store calls, which no isolation level can span.

   **Detect-and-retry was tried first, and it starves.** Keeping the two-call
   shape and having the adapter take a per-entity advisory lock, re-check the
   supersession targets inside its transaction, and report `ErrConflict` for a
   stale plan is *correct* — the invariant held under every test. It is also
   unusable: the writer that waits on the lock always finds its plan stale by
   the time it gets in, while the writer that never waits never conflicts, so
   the loser loses every round. Measured against two processes writing one
   entity: one writer landed 40 of 40 writes and the other landed **0 of 40**,
   reproducibly. Retrying and backing off make it worse, because they give the
   winner a longer head start.

   **The fix is to move the read inside the lock**, which means `Apply` takes a
   plan rather than a finished write:

   ```
   Apply(ctx, ApplyRequest{Entity, Valid, TxAt, Plan})
   Plan func(current []Record, txAt time.Time) (Write, error)
   ```

   The store locks the entity, reads its current overlapping records `FOR
   UPDATE`, calls the plan, and applies the result — one transaction, one lock,
   no window. chronicle's temporal reasoning still lives above the store, which
   never learns what a remainder is; it just runs where the store can protect
   it. `ErrConflict` and the retry loop survive for `StaticWrite`, which is not
   planned from the store's own read and so can still be stale.
3. **Transaction time should be assigned database-side** — a sequence, or
   `clock_timestamp()` inside a serializable transaction — rather than by the
   Go process. The in-memory implementation ratchets tx time forward per `Log`,
   which is only sound with a single writer; two `Log` values over one store
   each ratchet against their own history and can interleave.

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
- **Retention policies** — shipped, phase 3, as package `retain`. Per-kind
  schedules, enforced by an explicit sweeper with a first-class dry run
  (`Plan` vs `Execute`). Defaults ship as *unset*, because the commonly-cited
  periods do not apply the way vendors say: HIPAA's six years attaches to
  written policies and procedures (45 CFR 164.316(b)(1)), not audit logs; the
  SOX-lineage seven years (PCAOB AS 1215 .14, SEC Rule 2-06) binds the
  external audit firm's workpapers, not the issuer's database. 21 CFR 11.10(e)
  is *relative* — as long as the subject records require. Two decisions made
  in implementation: eligibility is measured from **TxTo**, not TxFrom — the
  age that matters is how long a record has been superseded, and aging from
  TxFrom would destroy a record that stopped being current belief yesterday —
  and a current record is never eligible at any age, enforced twice, in the
  sweeper and again in the store's `Delete`, which refuses whole batches.
  Deletion is a store *capability* (`Deleter`), an optional extension both
  shipped stores implement, so the core `Store` contract stays destruction-
  free and third-party stores are not broken by the addition.
- **Legal hold** — shipped, phase 3. Suspends retention deletion for scoped
  records. Hold always wins over retention. Critically, FRCP 37(e)'s trigger
  is "anticipation or conduct of litigation", determined after the fact by a
  court and **not** by complaint filing, so a hold must accept a **backdated,
  operator-asserted effective timestamp**. A hold that can only take effect
  "now" is the wrong shape for the obligation it exists to satisfy.

  **Correction, found during phase 3: "suspends deletion for scoped records
  from that moment" invites a wrong implementation, and the words above were
  nearly it.** The tempting reading is that `EffectiveFrom` filters *which
  records* the hold protects — records newer than the effective instant, on
  one axis or another. That reading destroys evidence: the preservation duty
  covers relevant information *however old it is*, so a hold scoped by its
  own effective date would sweep away exactly the records it was placed to
  keep. As shipped, `EffectiveFrom` gates only *when the hold is active* — a
  hold is in force over the half-open `[EffectiveFrom, ReleasedAt)`, and an
  active hold withholds every record in its kind/entity scope regardless of
  the record's timestamps. The backdated instant is an operator assertion for
  the record of controls; it also cannot resurrect anything destroyed before
  the hold was placed, and the design must not imply otherwise.
- **Tamper evidence** — shipped, phase 3, and still deliberately demoted:
  off by default, opt-in via `WithChaining`. Honest threat model, stated
  plainly on the option and in the README: a hash chain detects retrospective
  edits by someone who does **not** control the chain head. It does nothing
  against an administrator who owns the database and can recompute the entire
  chain. Only external anchoring changes that; `ChainHead` exposes the value
  to anchor, and chronicle ships no anchoring, so it must not imply the
  stronger guarantee. The canonical serialization is versioned (a leading
  format byte, a `v1:` token prefix) so a future change meets an explicit
  unknown-version divergence rather than a silent mismatch.

  **Correction, found during phase 3: tombstones preserve chain
  *verifiability*, not the full threat model, and the difference must be
  stated.** Retention under a chain retains each destroyed record's chain
  value as a tombstone, and `Verify` passes over the gap. Two things follow
  that "the chain still verifies" glosses over. First, a tombstone's own hash
  is unverifiable — the content it summarised is destroyed — so within a run
  of consecutive tombstones only the *last* one is constrained, by the first
  surviving successor; the others are carried on trust. Second, the store
  writes tombstones for whatever it is asked to delete, so an administrator
  with database access can destroy a chained record *through the tombstone
  protocol* and Verify passes exactly as it does after a legitimate sweep. A
  verified chain across a gap therefore proves the survivors are what the
  head commits to and that *something* with the recorded chain value stood in
  the gap — never that the destruction was authorised. Distinguishing
  authorised from unauthorised destruction requires records Verify cannot
  reach: externally anchored heads plus sweep reports kept out of the
  administrator's editorial reach.

  **Correction, found during phase 3: the record hash cannot cover TxTo, and
  "hash the immutable fields" hides a real gap.** TxTo is written *after* the
  hash, at supersession — the one mutation the model permits — so an editor
  who only shifts a superseded record's TxTo would go undetected by the hash
  alone. `Verify` compensates by requiring every superseded chained record's
  TxTo to equal the TxFrom of some *later chained write* (all of which are
  hash-covered). That pins TxTo to the set of instants the chain vouches for,
  but not to the right member of the set: moving a supersession from one
  chained write's instant to another's remains undetectable. Closing that
  residue would need per-supersession chain entries, which is a different and
  heavier design; the gap is documented rather than papered over.
- **Erasure** — GDPR Art.17 versus a non-destructive log. Shipped, phase 3,
  as mechanism only: per-subject AES-256-GCM under a pluggable `Keyring`,
  `DestroyKey` terminal per subject, `Get`/`Diff` failing with `ErrShredded`
  rather than returning ciphertext, `History` preserving record structure.
  The hash covers the *ciphertext*, so shredding never touches a chain — the
  simplest of the available constructions and the reason key destruction and
  tamper evidence compose without either weakening the other. Four research
  sweeps failed to verify whether any DPA, EDPB guidance or court has accepted
  destruction of a per-subject key as erasure. **Until that is resolved,
  chronicle documents the mechanism and hedges the legal characterization:**
  "destroying a key renders that subject's historical values unrecoverable;
  whether this constitutes erasure under Art.17 depends on your supervisory
  authority's position." No compliance claim the research does not support.

**Correction, found during phase 3: archive-before-delete cannot be
transactional through the Store interface, so the archive hook must be
idempotent — a requirement, not advice.** The archive-table strategy this
design pointed at was imagined as "copy, then delete, atomically". There is no
atomically: the caller's archive write runs in the caller's failure domain and
the deletion in the store's, and `Store` deliberately has no way to run caller
code inside a store transaction (that door was closed in phase 1 for good
reasons). If the archive succeeds and the deletion fails — or the process dies
between the two — the records are still in the store and the next sweep
archives them again. The contract is therefore: the hook runs *before*
destruction, its error aborts the batch untouched, and it **must** be
idempotent (key the archive on record ID; upsert, never append). A
double-archive on retry is the designed behaviour, because the alternative —
deleting before archiving — turns the same crash into data loss instead of a
duplicate row.

**Note, phase 3: chaining trades away the planner's narrowed read.** A chained
write must link from the entity's chain tail — the greatest current record in
the total order — which may not overlap the interval being written. So under
`WithChaining` the log asks the store for *all* of the entity's current
records (`ApplyRequest.Valid = Always()`) and filters to the overlapping ones
itself. The narrowing was an optimization and the store contract already
permitted the wider request; worth knowing when reasoning about the cost of a
chained write to an entity with many disjoint current intervals. The tail
read happens inside the store's lock, in the planner, which is what makes the
chain race-free across processes without any chain-specific locking.

## Phase 4: chronicled, the standalone service

The flexitype-pattern deployment: for polyglot shops that cannot import the
Go library, `chronicled/` is a nested module wrapping one `chronicle.Log`
over pgstore in a JSON REST API. The dependency budget is deliberate and
part of the claim — stdlib `net/http` with Go 1.22 pattern routing, the two
sibling modules, and `jackc/pgx/v5` as the driver. Nothing else: no router,
no config library, no logging library (`log/slog` is stdlib).

**The load-bearing decision is actor attribution.** An audit service that
accepts caller-supplied actor claims records fiction, so each static bearer
token maps in configuration to the `Actor` it writes as, and the service
stamps that actor on every write — record writes, hold placement
(`PlacedBy`) and hold release (`ReleasedBy`) alike. No request body carries
an actor; its presence is a 400 explaining why, not a silent ignore, and the
same rejection covers every transaction-time field. Token comparison is
constant-time (`crypto/subtle` over SHA-256 digests, no early exit). Two
roles: `writer` (write + read) and `admin` (also holds, retention sweeps,
shredding, chain verification). This is API-key auth for a single trust
zone, stated as such; anything bigger puts mTLS or OIDC in front, and the
service does not grow an identity provider.

Semantics the HTTP boundary must not erode, and how they are held: the
server assigns transaction time (no `txAt` accepted anywhere); an absent
`validTo` — or an explicit `null` — is the unbounded end; point lookups
default absent instants to *now* while the cross-entity query treats absent
filters as *no restriction*, preserving the library's deliberate `As`/`Query`
contrast; the pagination cursor passes through opaquely, tested by walking
the same store over HTTP and through the library and requiring identical
sequences; every error is `{error, code, detail?}` with codes mirroring the
sentinel taxonomy, mapped with `errors.Is`, and unmapped errors are a
generic 500 so no driver string leaks.

**Correction, found during phase 4: "one Log per process is safe because the
store assigns transaction time" is true, and proves more than it was used
for.** The phase brief pointed at horizontal scaling as the thing to worry
about — per-replica Logs over one database. That is the *safe* direction:
pgstore stamps every write from the database clock inside the write's own
transaction, no process's ratchet is authoritative, and any number of Logs
across any number of replicas produce one correctly ordered transaction
axis. The wrinkle is *within* a replica: a `Log` serializes its writes
(open question 8 above — `Log.mu` is held across the store call), so one
replica lands one HTTP write at a time whatever entity it names, while
pgstore's per-entity advisory lock would happily run disjoint entities
concurrently. And the very argument that makes replicas safe — the store
owns the transaction axis — equally licenses N Logs inside one process, so
the ceiling is self-imposed, not structural. chronicled ships one Log per
process anyway: an audit log's write path is rarely the bottleneck, chaining
is simplest to reason about with one writer per process, and a Log pool is a
mechanical change if a deployment ever measures the ceiling. The point of
this correction is that the spec's safety argument and its deployment advice
were the same sentence, and they are actually two claims with different
strengths — safety is proven, single-Log is merely chosen.

**Correction, found during phase 4: requiring `validFrom` over HTTP makes
one corner of the library's model unreachable, and the asymmetry with
`validTo` should be stated rather than discovered.** The wire contract
requires `validFrom` while treating an absent or null `validTo` as
unbounded. The library itself accepts a zero `ValidFrom` — "this fact was
always true" — which the service therefore cannot express. The tightening is
deliberate: in a JSON body an absent `validFrom` is overwhelmingly a
forgotten field, not an assertion about all of history, and JSON decoding
cannot distinguish absent from null to give the two different meanings. But
it is an expressiveness loss, the openapi description says so, and if a
deployment ever needs since-forever assertions over HTTP the right shape is
an explicit sentinel (an `"unbounded": true` companion field, or accepting
the literal string), never a defaulting absent field.

A related trap the review surfaced: Go's zero time renders in RFC 3339 as
`0001-01-01T00:00:00Z`, which is exactly the value the library reads as
unbounded/now. A caller sending that literal string could reach the sentinel
by accident — a `validTo` that looks inverted but silently means "still
holds", a `validAt` that silently means "now". The service therefore rejects
the zero rendering on every timestamp field with a 400, so the only way to
mean unbounded stays "omit the field", consistent with the reasoning above.
Caller valid times are also truncated to the store's microsecond resolution
at the boundary, so the record echoed in a write response is byte-identical
to what a later read returns rather than the nanosecond input the store
never held.

Two smaller notes, recorded rather than papered over. First, the retention
sweep endpoint computes `now` from the service clock while hold releases and
supersessions are stamped by the database clock; retain's documented skew
caveat therefore applies at the HTTP boundary, and the integration suite
actually tripped over it (a just-released hold still withholding for the
milliseconds the database clock ran ahead). Retention periods dwarf the skew
in production; tests sleep past it. Second, the fixed endpoints
(`/v1/records`, `/v1/holds`, `/v1/retention/…`, `/v1/subjects/…`) and the
entity tree (`/v1/{kind}/{entity}/…`) are kept disjoint by segment count and
literal precedence in the mux — no request is ambiguous — but an entity kind
literally named `records` or `holds` will read oddly in URLs. The service
does not reserve those kinds; the spec documents the namespace instead.

Operationally: environment-only config failing fast with actionable
messages; `Migrate` on boot strictly opt-in (schema changes in production
should be explicit acts, and pgstore's `SchemaSQL`/`KeysSchemaSQL` exist for
migration tools); graceful drain on SIGTERM/SIGINT before the DB closes;
one structured log line per request with method, path, status, duration and
actor ID — never the token, and never a request body, because bodies are
the regulated data itself. The Docker image is a multi-stage
`CGO_ENABLED=0` build into `distroless/static-debian12:nonroot`, proving
the static-binary claim end to end, with a compose file for the
one-command demo. The OpenAPI document is hand-written, embedded via
`go:embed`, served authenticated, and a test fails if any routed path is
missing from it.

## Phase 5: field history

`FieldHistory(ctx, kind, id, path, as, opts...)` is the single-field audit
trail — "how did our recorded belief about entity E's field X change over time,
and who changed it". It is the read the *Reads* table above promised and had
not delivered, and it is what Salesforce sells as Field Audit Trail.

**It is built as a read-side composition, and that is the whole design.** It
walks the entity's records that cover a fixed valid point — a single
`Store.Query` with the `ValidAt` filter, paged internally — decodes each through
the log's `Codec`, and reports each transaction step at which the value at
`path` differs from the step before, by the *same* comparison `Diff` uses. It
adds no `Store` method, no column, no migration, and touches nothing on the
write path. Because it composes from capabilities both stores already have, it
works identically on `MemStore` and pgstore and the conformance suite exercises
it on both with no new store surface. The one new sentinel, `ErrInvalidPath`
(with `*PathError`), distinguishes a malformed pointer from a well-formed one
that matches nothing; the RFC 6901 machinery lives in one file, `pointer.go`,
shared with the path grammar `Diff` already emits.

The bitemporal framing the brief insisted on is the correct one, and the
implementation holds it exactly: `as.ValidAt` pins the point in the world and
the walk runs along the *transaction* axis, not valid time. `as.TxAt` is
ignored — the mirror image of `Timeline`, which uses only `TxAt` — and that is
documented rather than left as a surprise.

**Correction, found during phase 5: the brief's present→absent mechanism does
not exist.** The brief said a field goes absent when "a later belief bounds
validTo before ValidAt". It does not. An ordinary `Put`/`Correct` never reduces
an entity's valid coverage: a write whose interval stops before the queried
point still leaves the superseded record's tail as a *remainder* carrying the
old value across the point, so the point stays covered — by the old belief, not
by absence. Total valid coverage after any write is the union of the old
coverage and the new interval; it only ever grows. The present→absent
transition is real but arrives another way: a later belief that still covers the
point but whose object no longer *contains* the field — a correction that omits
it, or a `null`-body tombstone (JSON `null` decodes to an empty object). That
belief is a genuine record with an author and an instant, so the transition is
attributed cleanly. Genuine coverage *lapse* — `Get` returning nothing at a
point that was covered — is unreachable through the public write API; only
destruction (retention) or erasure (shredding) can remove a belief, and
`FieldHistory` reflects surviving history rather than resurrecting what was
destroyed. The test that would have "proved" the brief's version passes for the
wrong reason; the shipped test pins the actual behaviour.

**Correction, found during phase 5: the name `FieldChange` was already taken.**
The brief named the result element `FieldChange`, but that identifier is the
element of a `Delta` — a *spatial* diff between two states, carrying an
add/remove/modify `Op`. A field's history is a different shape: no `Op` (every
element is a change by construction), an explicit presence flag on each side so
absent stays distinct from null, and the attribution and both time coordinates
of the introducing belief. Overloading one name with two meanings would have
been the more confusing choice, so the element is `FieldRevision` and its value
type is `FieldValue{Value, Present}`. The `absent`-vs-`null` distinction the
brief flagged as the classic subtle bug is carried by that `Present` bool, and
tested both directions (set→null is a change; null→dropped is a further change).

Cost is linear in the number of beliefs ever recorded about the fixed valid
point — one decode each — and independent of the rest of the log; the internal
query is paged so a deep transaction history is never held in memory at once. A
codec failure at any step is `ErrCodec` and a shredded belief is `*ShredError`,
never a silently skipped step — the same posture `Diff` takes, for the same
reason: a change log that under-reports is worse than one that fails.

The service exposes it at `GET /v1/{kind}/{entity}/field-history?path=&validAt=&
descending=`: `path` required (a 400 that explains, URL-decoded, NUL-rejected
like every other read path), `validAt` optional and defaulting to now with the
zero-rendering sentinel rejected, each change rendering `from`/`to` as raw JSON
with a `present` flag so absent is an omitted value rather than a JSON null.
It is a read, so `writer` or `admin` may call it, and `ErrInvalidPath` maps to
400 `invalid_path` in the existing taxonomy.

## Failure modes this design answers

From practitioner reports of hand-rolled systems:

| Failure | chronicle's answer |
|---|---|
| WAL/CDC tailing loses actor identity | `Actor` is **required** on every write and has no defaulting path, so it cannot silently degrade to "system" |
| WAL/CDC tailing loses business intent | Partly. `Intent` (assert / correct / remainder) is always recorded. `Reason` is free text and **optional** — see COMPLIANCE.md — so it captures intent only where callers supply it. A field you do not require cannot be relied upon; chronicle does not pretend otherwise |
| Trigger shadow tables drift from schema | History is codec-serialized, not a mirrored column set |
| Event streams can't reconstruct point-in-time | Full-row records with as-of on both axes |
| Audit table outgrows the primary | Retention sweeper (`retain`) plus caller-owned archival via the archive hook. *Not* tx-time partitioning — the phase 2 Correction above proved that incompatible with the exclusion constraint, and this row originally promised it anyway |
| "Who changed this" across jobs/migrations | Actor is required; no ambient default that silently records "system" |
| Schema evolution orphans history rows | Records carry their own shape; readers get the shape as written |

## Open questions

1. Codec — JSON first. `Data []byte` keeps it pluggable, but the *query by
   changed field* path needs structured access, so Postgres `jsonb` is the
   likely concrete floor. **Narrowed in phase 2, still open.** `jsonb` cannot
   be the storage type for `Data`: `Record.Data` is opaque bytes under a
   pluggable `Codec`, and a `jsonb` column would reject every non-JSON codec
   outright — turning a storage adapter into a codec mandate. The adapter
   stores `data bytea` and keeps `Codec` meaning what it says. Query-by-changed-
   field therefore needs a JSON *projection* alongside the authoritative bytes —
   a generated column guarded by a check, or a side table — rather than a change
   of primary storage. `Meta` is `jsonb`, because chronicle controls its shape
   entirely and a GIN index over it is one statement away.
2. ~~Does `Correct` need to be storage-distinct from `Put`?~~ **Answered:** no.
   An `Intent` flag on the record is sufficient and shipped.
3. ~~Non-overlap on the transaction axis?~~ **Answered, with a precondition.**
   Tx-time overlap is impossible by construction only when a *single* writer
   assigns transaction time. That holds for one `Log`; it does not hold across
   processes, which is why the SQL adapter must assign tx time database-side.
4. ~~Structural diffing of nested records.~~ **Answered:** implemented in full
   — nested objects and arrays to any depth, RFC 6901 paths with `~0`/`~1`
   escaping, shape changes reported once at the node rather than as an
   add/remove burst, and `json.Number` so large integers compare exactly.
   Known limitation, documented and tested rather than papered over: **arrays
   compare by position**, so inserting at the head reports every later element
   as modified. An LCS or identity-field heuristic guesses at caller intent;
   a stated rule is more honest.
5. Remainder records carry the *superseded* record's actor, reason and
   meta, not the splitting writer's — otherwise the log would claim someone
   asserted data they never sent. Attribution is not lost: remainders share
   `TxFrom` with the write that caused them, so the assert/correct record at
   that instant identifies who split it.
6. **New, phase 2. Timestamp resolution is not uniform across stores.**
   Postgres `timestamptz` holds microseconds; Go's `time.Time` holds
   nanoseconds. Transaction time is assigned by the database and so is already
   microsecond-aligned, but caller-supplied *valid* times are truncated on the
   way in, and round-trip equality holds only to the microsecond. The
   conformance suite works in whole seconds so that resolution is never the
   thing under test. Whether the contract should *require* nanosecond fidelity —
   forcing a second column, or a different storage type — is open; nothing in
   the temporal semantics needs it, and no adapter would enjoy it.
   **Phase 3 found a third stakeholder:** the hash chain's canonical
   serialization must commit to the *coarsest* resolution the storage contract
   guarantees, or verification fails against exactly the stores it is meant to
   protect. Canonical times are therefore microsecond-truncated, which means a
   sub-microsecond edit to a caller-supplied valid time is invisible to the
   chain — the same way it is invisible to a round trip through Postgres. If
   the resolution contract ever tightens, the chain format version must bump
   with it.
7. **New, phase 2. Record IDs are unique across processes but no longer track
   transaction order across them.** An ID leads with the minting log's proposed
   transaction instant, and the store may assign a different one. Uniqueness is
   guaranteed by a per-log random token — without it two processes could mint
   the same ID and a primary key would silently swallow one of two concurrent
   writes. Ordering by ID is only used as the final tiebreak between records
   that already share a transaction instant and a valid start, which in
   practice means records written together, so a within-log ordering suffices.
   Documented on `RecordID`; worth revisiting if IDs ever become a public sort
   key rather than an internal tiebreak.
8. **New, post-phase-3 review. One `Log` holds its write lock across the store
   call, so writes to different entities do not proceed concurrently through a
   single `Log`.** The store level is fine — pgstore's advisory lock is
   per-entity — but `Log.mu` is held from ratchet tick to `Apply` return,
   which serializes the whole process's writes and makes "writes to different
   entities do not contend" false end-to-end. The documented answer today is
   one `Log` per worker over a store that assigns transaction time (safe by
   construction; the ratchet then follows the store). The open question is
   whether the lock can be narrowed instead — held only for the tick and the
   ID mint, with `Apply` outside it. The hazards to resolve before trying:
   the ratchet adoption after `Apply` (`lastTx` must never move backwards
   while another write is mid-flight), the meaning of per-log sequence
   numbers once writes interleave, and `MemStore`, whose adoption of the
   log's proposal assumes the proposals arrive in order. Not attempted in the
   review that found it; a doc-only correction shipped instead.
