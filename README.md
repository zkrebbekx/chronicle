# chronicle

Bitemporal entity change history for Go — an ORM-agnostic, queryable record of
what your entities were, and of what you believed they were at any point in the
past.

Every enterprise product reimplements entity history, and most get the same
thing wrong: they keep one time axis. That answers "what is Alice's salary" and
"what was it in March", and then answers "what did we believe in April that her
March salary had been" with today's belief about March — confidently, and
wrongly, because a retroactive correction rewrote what the system appears to
have known. That third question is the one that settles audits and disputes,
and it needs a second axis.

- **Two independent time axes.** *Valid time* is when a fact was true in the
  world and you supply it. *Transaction time* is when chronicle learned it; the
  system assigns it, you cannot set it, and it is never rewritten. There is no
  exported way to write the transaction axis, which is the whole basis for
  trusting the log.
- **Non-destructive by construction.** Nothing is ever updated in place or
  deleted. An overlapping write closes the superseded records' transaction
  interval and writes replacements — which is exactly what 21 CFR 11.10(e)
  means by "record changes shall not obscure previously recorded information",
  with no cryptography involved.
- **A real query surface.** Filter by kind, entity, actor, intent and time
  range on *either* axis, with deterministic keyset pagination behind an opaque
  cursor. The nearest Go alternative's entire storage interface is
  `Store/Get/Has/Clear` on one opaque string, with no time parameter anywhere.
- **Structural field-level diffs.** Nested objects and arrays to any depth,
  RFC 6901 JSON Pointer paths, exact number comparison. A codec failure is an
  error, never a silently empty diff.
- **Required actor attribution.** No ambient default, no silent "system".
- **Zero dependencies.** Standard library only. Go 1.23+.

```go
import "github.com/zkrebbekx/chronicle"
```

> **Status: phase 1.** This is the core model and the in-memory store. The
> Postgres adapter, the REST service, and the retention / legal-hold /
> tamper-evidence layer are later phases. See [docs/DESIGN.md](docs/DESIGN.md).

## The question that justifies the library

```go
log := chronicle.NewLog(chronicle.NewMemStore())
hr := chronicle.Actor{ID: "u-42", Name: "Dana"}

// In March, we record that Alice earns 50000 effective 1 March.
first, _ := log.Put(ctx, "employee", "alice",
    []byte(`{"salary":50000}`), march, time.Time{}, hr)

// In April, we discover the figure was wrong — it was always 60000.
log.Correct(ctx, "employee", "alice",
    []byte(`{"salary":60000}`), march, time.Time{}, hr)

now, _  := log.Get(ctx, "employee", "alice", chronicle.ValidAt(march))
then, _ := log.Get(ctx, "employee", "alice",
    chronicle.As{ValidAt: march, TxAt: first.TxAt})

now.Data  // {"salary":60000}  — what we believe today about March
then.Data // {"salary":50000}  — what we believed in March about March
```

Both answers are correct, and they are different. A uni-temporal log can only
give you the first.

## Half-open intervals, and the zero time

Both axes are half-open `[from, to)`. An unbounded end is the **zero
`time.Time`**, never a sentinel maximum — zero is unambiguous in Go, survives
marshalling, cannot collide with a real instant, and maps to SQL `NULL`.

A zero `ValidTo` means the fact still holds. A zero `ValidFrom` means it always
did. A zero `TxTo` means the record is current belief.

Reading a zero time correctly depends on which end of an interval it sits at —
a zero lower bound is −∞ and a zero upper bound is +∞. Getting that backwards
is *the* bitemporal bug, so all of it lives in one type:

```go
chronicle.Between(march, june)  // [march, june)
chronicle.Since(march)          // [march, ∞)
chronicle.Until(june)           // [-∞, june)
chronicle.Always()              // all of time; the zero Interval

chronicle.Between(march, june).Overlaps(chronicle.Since(june)) // false — adjacent
```

Use `Interval`'s methods rather than comparing record timestamps yourself.

## Writes split intervals

`Put` and `Correct` run the same algorithm and are identical in storage; they
differ only in the recorded `Intent`, which is what makes a retroactive fix
distinguishable from an ordinary late-arriving fact.

```go
log.Put(ctx, "employee", "alice", []byte(`{"grade":"L3"}`), march, time.Time{}, hr)
log.Put(ctx, "employee", "alice", []byte(`{"grade":"L4"}`), april, june, hr)

// [2026-03-01, 2026-04-01)  {"grade":"L3"}
// [2026-04-01, 2026-06-01)  {"grade":"L4"}
// [2026-06-01, ∞)           {"grade":"L3"}
```

The uncovered parts of the superseded record are rewritten as **remainders**.
A remainder carries the *superseded record's* actor, reason and metadata, and
is marked `IntentRemainder` — it re-asserts a fact its original author
asserted, and stamping the splitting writer on it would have the log claim they
said something they never said. Nothing is lost: a remainder shares its
`TxFrom` with the write that produced it, so the `IntentAssert` or
`IntentCorrection` record at that same instant identifies who caused the split.

**The invariant, which is the point of the library:** at any transaction
instant, an entity's current valid intervals do not overlap, and no write
introduces a gap where the timeline was previously covered. This is asserted
after every step of a seeded property test over long randomised write
sequences, and under `go test -fuzz`.

## Transaction time is ratcheted

Two writes must never share a transaction instant — a record superseded by the
write immediately following it would be left with an empty transaction interval
that no as-of query could ever observe.

So chronicle ratchets: a write whose clock reading fails to advance on the
previous one is assigned **the previous instant plus one nanosecond**.
Transaction timestamps within a `Log` are strictly increasing whatever the
clock does, including a frozen test clock or a clock that jumps backwards.

The alternative — letting timestamps tie and ordering on a separate sequence
number — pushes the tiebreak into every reader and every downstream query.
Ratcheting keeps it in one place. The cost is that transaction time can lead
the wall clock by a nanosecond per write under sustained load, which
self-corrects as soon as the write rate drops. The ratchet is per-`Log`, so run
one `Log` per store.

## Reads

```go
log.Get(ctx, kind, id, as)                 // one record, both axes
log.History(ctx, kind, id, opts...)        // every version ever, superseded included
log.Timeline(ctx, kind, id, as)            // valid-time sequence at one belief instant
log.Diff(ctx, kind, id, from, to)          // field-level changes between two points
log.Query(ctx, q)                          // cross-entity, filtered, paginated
```

`As{ValidAt, TxAt}` locates a point in bitemporal space; a zero field means
"now". `chronicle.Now()`, `ValidAt(t)` and `AsOf(t)` cover the common cases.

Pagination is keyset, ordered by transaction start, then valid start, then the
unique record ID. Because the ID is the final tiebreaker the order is total, so
no row can be skipped or repeated at a page boundary however many records share
a timestamp:

```go
var cursor chronicle.Cursor
for {
    page, next, err := log.Query(ctx, chronicle.Query{
        Kind: "employee", ActorID: "u-42", Limit: 100, After: cursor,
    })
    if err != nil {
        return err
    }
    // ... use page ...
    if next.IsZero() {
        break
    }
    cursor = next
}
```

The cursor is opaque, URL-safe and checksummed; a mangled one is
`ErrInvalidCursor` rather than a silently wrong page.

## Diffing

`Diff` decodes record data through a `Codec` (JSON by default) and compares the
decoded structures, reporting each change with an RFC 6901 JSON Pointer path.

```go
// modified /address/city    Leeds -> York
// modified /salary          50000 -> 60000
// added    /tenured         <nil> -> true
```

It descends into nested objects and arrays to any depth. A change of *shape* at
a node — an object becoming a scalar — is reported once at that node rather
than as a burst of unrelated additions and removals. Objects compare by key, so
reordering keys is not a change. Numbers decode as `json.Number`, so integers
beyond `float64`'s exact range compare correctly.

**Documented limitation:** arrays are compared **by position**. Inserting an
element at the head of an array reports every later element as modified plus
one addition at the end, rather than a single insertion. Reporting it as an
insertion needs an alignment heuristic — an LCS over values, or a per-array
identity field — and a heuristic that misfires on the cases it does not fit
would be worse than a rule that is simple and stated.

A codec failure is `ErrCodec`, never an empty diff. A change log that reports
"nothing changed" when it means "I could not tell" is worse than one that
fails.

## Storage

```go
type Store interface {
    Put(ctx context.Context, recs []Record) error
    Supersede(ctx context.Context, ids []RecordID, txTo time.Time) error
    Get(ctx context.Context, q GetQuery) (*Record, error)
    Query(ctx context.Context, q Query) ([]Record, Cursor, error)
}

type Atomic interface { // optional, and strongly preferred
    Apply(ctx context.Context, w Write) error
}
```

A write supersedes some records and inserts others, and the two must land
together or a reader sees a gap or an overlap. The four `Store` methods cannot
express that — `Supersede` and `Put` are separate calls with no shared
transaction — so stores should also implement `Atomic`. chronicle uses it
whenever it is present and falls back to a non-atomic pair when it is not.

`MemStore` implements both and is safe for concurrent use. A SQL implementation
should run `Apply` in one transaction and, because chronicle reads an entity's
overlapping records before computing the write, should take row locks or run
serializable so that two writers to one entity cannot both split the same
pre-state.

## Compliance, honestly

See [docs/COMPLIANCE.md](docs/COMPLIANCE.md), which is sourced to primary
regulatory text rather than vendor marketing. The short version:

**No regulation surveyed there textually requires cryptographic
tamper-evidence, immutability, WORM storage, hash chaining or Merkle trees.**
`21 CFR 11.10` contains no occurrence of *tamper-evident*, *hash*, *immutable*
or *WORM*. The genuine bar is *non-destructive* — 11.10(e)'s "record changes
shall not obscure previously recorded information" — which full-row versioning
satisfies with no cryptography at all, and which FDA confirmed explicitly in
the 1997 final-rule preamble (62 FR 13430, Comment 111).

Consequences visible in this API:

- **`Actor` is required**, with no ambient default. 11.10(e) records "operator
  entries and actions"; 11.50(a)(1) requires the signer's printed name; PCAOB
  AS 1215 .16 requires "the name of the person who prepared".
- **`Reason` is optional.** The "who/what/when/why" formulation vendors
  attribute to Part 11 is not in the regulation. The one clear
  reason-for-change mandate in the researched corpus binds audit firms'
  workpapers, not your database.
- **No default retention period.** The commonly cited periods bind other
  parties or other objects — HIPAA's six years attaches to written policies
  (45 CFR 164.316(b)(1)), and the SOX-lineage seven years binds the external
  audit firm (PCAOB AS 1215 .14, SEC Rule 2-06). 21 CFR 11.10(e) is *relative*.

chronicle is a library; compliance is a property of your whole system. This is
not legal advice.

## Non-goals

- **Not a database.** Postgres does the hard storage work; chronicle does not
  reimplement MVCC, indexing or replication.
- **Not a CDC/WAL tailer.** Those lose actor identity and business intent — the
  two things a change record exists to capture.
- **Not event sourcing.** chronicle records what an entity *was*, not a stream
  of domain commands to fold.
- **No ORM, ever.** That is the specific unoccupied axis, and the reason every
  incumbent is unusable outside its own framework.
- **No tamper-evidence claims.** A hash chain detects retrospective edits by
  someone who does not control the chain head. It does nothing against an
  administrator who owns the database and can recompute it. Only external
  anchoring changes that, and chronicle does not ship anchoring, so it will not
  imply the stronger guarantee.

## License

MIT
