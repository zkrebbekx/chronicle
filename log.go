package chronicle

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Log is the bitemporal engine: it turns caller assertions about what was true
// into a non-destructive record of what was believed, and when.
//
// A Log is safe for concurrent use, and serializes its writes: the write path
// holds the log's lock across the store call, so one write is in flight per
// Log at a time, whatever entity it names. Reads never take the write lock.
// Where write throughput across entities matters, run one Log per worker over
// a store that assigns transaction time itself — pgstore does — which is safe
// because the store, not any one log, orders the transaction axis. A
// [MemStore] adopts the log's instants instead, so it must have exactly one
// Log writing to it.
//
// # Transaction time
//
// Transaction time is assigned here and nowhere else. There is no exported
// field, option, or argument by which a caller can set TxFrom or TxTo, and
// that absence is load-bearing: a log whose transaction axis can be written by
// its users records what someone wanted to have believed, not what was
// believed, and answers the audit question wrongly while looking correct.
//
// # The monotonic ratchet
//
// Each write takes the clock's instant, but the log will not accept one that
// fails to advance: if the clock returns an instant at or before the previous
// write's, the log uses the previous instant plus one nanosecond instead.
// Transaction timestamps within a log are therefore strictly increasing.
//
// This settles the same-instant question the cheap way round. Two writes in
// the same nanosecond — from a coarse system clock, a frozen test clock, or
// simple speed — get distinct, ordered transaction times, so a superseded
// record always has TxTo strictly after TxFrom and is never left with an empty
// transaction interval that no as-of query could see. The alternative, letting
// timestamps tie and ordering on a sequence number, makes every reader carry
// the tiebreak; ratcheting puts it in one place.
//
// The cost is that transaction time can run ahead of the wall clock under
// sustained writes, by one nanosecond per write beyond the clock's resolution.
// At a million writes per second that is a millisecond of drift per second of
// writing, and it is self-correcting the moment the write rate drops.
//
// The ratchet is per-Log, and a Log's clock is only authoritative when it is
// the only writer. [Store.Apply] therefore returns the transaction instant it
// actually assigned, and the log adopts it: a store shared between processes
// stamps its own instant — from the database's clock, not any one process's —
// and the ratchet follows along behind it. Two Log values over one Postgres
// store consequently produce a single, correctly ordered transaction history,
// which two independent in-process ratchets could not.
type Log struct {
	store   Store
	clock   Clock
	codec   Codec
	kinds   map[string]struct{}
	node    string
	retries int
	chain   bool
	keyring Keyring

	mu     sync.RWMutex // guards lastTx and seq; held across the whole write path
	lastTx time.Time
	seq    uint64
}

// Option configures a [Log].
type Option func(*Log)

// WithClock sets the clock supplying transaction time. The default is
// [SystemClock]. A clock cannot be used to backdate a write: whatever it
// returns is still forced strictly forward of the previous write, so an
// injected clock can slow the transaction axis down but never rewind it.
func WithClock(c Clock) Option {
	return func(l *Log) {
		if c != nil {
			l.clock = c
		}
	}
}

// WithCodec sets the codec used by [Log.Diff]. The default is [JSONCodec].
func WithCodec(c Codec) Option {
	return func(l *Log) {
		if c != nil {
			l.codec = c
		}
	}
}

// WithKinds restricts the log to a fixed set of entity kinds. Writes and reads
// naming a kind outside the set fail with [ErrUnknownKind].
//
// Without it any non-empty kind is accepted. The allow-list is worth setting
// where kinds come from anywhere near user input, since a typo'd kind
// otherwise creates a silently separate history that reads as an empty one.
func WithKinds(kinds ...string) Option {
	return func(l *Log) {
		if l.kinds == nil {
			l.kinds = make(map[string]struct{}, len(kinds))
		}
		for _, k := range kinds {
			if k != "" {
				l.kinds[k] = struct{}{}
			}
		}
	}
}

// WithWriteRetries sets how many times a write is recomputed after the store
// reports [ErrConflict]. The default is [DefaultWriteRetries]; a negative value
// is treated as zero, which makes the first conflict fatal.
//
// A conflict means another writer changed the entity between this log's
// overlap scan and its Apply. Retrying re-reads and re-splits, so the second
// attempt plans against the state the winner left behind. Raise this where
// contention on a single entity is expected and lower it where a caller would
// rather fail fast than sit in a queue.
func WithWriteRetries(n int) Option {
	return func(l *Log) {
		if n < 0 {
			n = 0
		}
		l.retries = n
	}
}

// DefaultWriteRetries is the number of times a write is recomputed after a
// store reports [ErrConflict].
const DefaultWriteRetries = 8

// NewLog returns a log over the given store. It panics if store is nil, since
// a log without storage has no meaningful degraded behaviour.
//
// Writes are applied through [Store.Apply], indivisibly. There is no
// non-atomic path: a supersession and the insertion it accompanies always land
// together or not at all.
func NewLog(store Store, opts ...Option) *Log {
	if store == nil {
		panic("chronicle: NewLog requires a non-nil Store")
	}
	l := &Log{
		store:   store,
		clock:   SystemClock,
		codec:   JSONCodec{},
		node:    newNodeToken(),
		retries: DefaultWriteRetries,
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// newNodeToken returns a short random token distinguishing this Log from every
// other one, including those in other processes.
//
// Record IDs are minted from a transaction timestamp and a per-log sequence
// number, and neither is unique across processes: two logs writing to one
// Postgres store in the same nanosecond, each at the same point in its own
// sequence, would mint the same ID, and the store's primary key would then
// silently discard one of the two writes. The token closes that hole. It is
// not a security boundary and does not need to be unguessable, only distinct.
func newNodeToken() string {
	var b [5]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any supported platform, and a log that
		// refused to start because of it would be worse than one whose IDs are
		// merely time-and-sequence unique within the process.
		return "0000000000"
	}
	return hex.EncodeToString(b[:])
}

// Codec returns the log's codec.
func (l *Log) Codec() Codec { return l.codec }

// WriteOption carries the optional parts of a write.
type WriteOption func(*writeOpts)

type writeOpts struct {
	reason  string
	meta    map[string]string
	subject string
}

// WithReason attaches a free-text business justification.
//
// Optional by design. Vendors routinely present "who, what, when and why" as a
// 21 CFR Part 11 requirement; the regulation's text does not contain it, and
// the one clear reason-for-change mandate in the researched corpus (PCAOB
// AS 1215 .16) binds audit firms' workpapers rather than the systems chronicle
// records. See docs/COMPLIANCE.md. Record a reason because your process wants
// one, not because a library told you a regulation demands it.
func WithReason(reason string) WriteOption {
	return func(o *writeOpts) { o.reason = reason }
}

// WithMeta attaches metadata. Later calls merge into earlier ones, and the map
// is copied, so the caller may reuse or mutate it afterwards.
func WithMeta(meta map[string]string) WriteOption {
	return func(o *writeOpts) {
		if len(meta) == 0 {
			return
		}
		if o.meta == nil {
			o.meta = make(map[string]string, len(meta))
		}
		maps.Copy(o.meta, meta)
	}
}

// WithMetaValue attaches a single metadata key.
func WithMetaValue(key, value string) WriteOption {
	return func(o *writeOpts) {
		if key == "" {
			return
		}
		if o.meta == nil {
			o.meta = make(map[string]string, 1)
		}
		o.meta[key] = value
	}
}

// Result describes what a write did. It is worth keeping: the superseded IDs
// and the transaction instant together are enough to reconstruct the write
// later, and the transaction instant is the coordinate an as-of query needs to
// see the state as it stood immediately after.
type Result struct {
	// TxAt is the transaction instant assigned to the write. Every record in
	// Written has this as its TxFrom, and every record named in Superseded has
	// it as its TxTo.
	TxAt time.Time
	// Written holds the records inserted: the caller's record first, then any
	// remainders.
	Written []Record
	// Superseded names the records whose transaction interval this write
	// closed.
	Superseded []RecordID
	// Record is the caller's own record, as stored.
	Record Record
}

// Put asserts that the entity had the given state over the given valid
// interval, as of now in transaction time.
//
// validFrom is inclusive and validTo is exclusive; a zero validTo means the
// state still holds, and a zero validFrom means it always did. An interval
// that is empty or inverted is rejected with [ErrInvalidInterval] rather than
// stored. The actor is required: a zero actor ID is [ErrMissingActor].
//
// Nothing is destroyed. Every current record whose valid interval overlaps the
// new one has its transaction interval closed, and where such a record extends
// beyond the new interval on either side, the uncovered part is rewritten as a
// remainder record carrying the superseded record's data. The result is that
// at the new transaction instant the entity's current records tile its valid
// timeline exactly: no overlaps, and no gaps that were not already there.
//
// # Attribution of remainders
//
// A remainder carries the *superseded record's* actor, reason and metadata,
// not the actor of the write that caused the split, and is marked
// [IntentRemainder]. The reasoning is that a remainder re-asserts a fact its
// original author asserted; stamping the new actor on it would have the log
// claim they said something they never said, which is the specific failure an
// attribution trail exists to prevent. Nothing is lost by this: a remainder
// shares its TxFrom with the write that produced it, so the record carrying
// [IntentAssert] or [IntentCorrection] at that same instant identifies who
// caused the split.
func (l *Log) Put(ctx context.Context, kind, entityID string, data []byte, validFrom, validTo time.Time, actor Actor, opts ...WriteOption) (Result, error) {
	return l.write(ctx, kind, entityID, data, Interval{From: validFrom, To: validTo}, actor, IntentAssert, opts)
}

// PutInterval is [Log.Put] taking an [Interval] rather than two instants. It
// is the same operation; the argument list is just shorter to read at a call
// site that already has an interval in hand.
func (l *Log) PutInterval(ctx context.Context, kind, entityID string, data []byte, valid Interval, actor Actor, opts ...WriteOption) (Result, error) {
	return l.write(ctx, kind, entityID, data, valid, actor, IntentAssert, opts)
}

// Correct records that a previously held belief was wrong.
//
// Its effect on storage is identical to [Log.Put] — same supersession, same
// remainders, same non-destructive guarantee — and it differs only in marking
// the record [IntentCorrection]. That flag is the point: without it, a
// retroactive fix is indistinguishable from an ordinary late-arriving fact,
// and "when did we discover we were wrong" has no answer even though every
// byte needed to answer it is present.
func (l *Log) Correct(ctx context.Context, kind, entityID string, data []byte, validFrom, validTo time.Time, actor Actor, opts ...WriteOption) (Result, error) {
	return l.write(ctx, kind, entityID, data, Interval{From: validFrom, To: validTo}, actor, IntentCorrection, opts)
}

// CorrectInterval is [Log.Correct] taking an [Interval].
func (l *Log) CorrectInterval(ctx context.Context, kind, entityID string, data []byte, valid Interval, actor Actor, opts ...WriteOption) (Result, error) {
	return l.write(ctx, kind, entityID, data, valid, actor, IntentCorrection, opts)
}

// write is the whole mutation path. Every exported write funnels through here,
// so the supersede-and-split algorithm exists in exactly one place.
func (l *Log) write(ctx context.Context, kind, entityID string, data []byte, valid Interval, actor Actor, intent Intent, opts []WriteOption) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := l.checkKind(kind); err != nil {
		return Result{}, err
	}
	if entityID == "" {
		return Result{}, ErrMissingEntityID
	}
	if actor.ID == "" {
		return Result{}, ErrMissingActor
	}
	// NUL is rejected here for the same reason it is rejected in metadata
	// below: a text column cannot hold it, so a write MemStore accepted would
	// fail on pgstore with a driver error. One behaviour, everywhere.
	for _, f := range [...]struct{ name, s string }{
		{"kind", kind},
		{"entity ID", entityID},
		{"actor ID", actor.ID},
		{"actor type", actor.Type},
		{"actor name", actor.Name},
	} {
		if strings.ContainsRune(f.s, 0) {
			return Result{}, fmt.Errorf("chronicle: %s contains a NUL byte: %w", f.name, ErrInvalidField)
		}
	}
	valid = valid.UTC()
	if err := valid.Validate(); err != nil {
		return Result{}, &IntervalError{Field: "valid", Interval: valid, Err: ErrInvalidInterval}
	}

	var o writeOpts
	for _, opt := range opts {
		opt(&o)
	}
	if strings.ContainsRune(o.reason, 0) {
		return Result{}, fmt.Errorf("chronicle: reason contains a NUL byte: %w", ErrInvalidField)
	}
	for k, v := range o.meta {
		if strings.HasPrefix(k, MetaReservedPrefix) {
			return Result{}, fmt.Errorf("chronicle: metadata key %q: %w", k, ErrReservedMeta)
		}
		// NUL is rejected here, at the boundary, rather than left to the
		// store: jsonb cannot hold a NUL inside a string, so a write MemStore
		// accepted would fail on pgstore with a raw driver error. One
		// behaviour, everywhere, decided by the library.
		if strings.ContainsRune(k, 0) {
			return Result{}, fmt.Errorf("chronicle: metadata key %q contains a NUL byte: %w", k, ErrInvalidMeta)
		}
		if strings.ContainsRune(v, 0) {
			return Result{}, fmt.Errorf("chronicle: metadata value under key %q contains a NUL byte: %w", k, ErrInvalidMeta)
		}
	}
	if o.subject != "" {
		var err error
		if data, err = l.sealForSubject(ctx, &o, kind, entityID, data); err != nil {
			return Result{}, err
		}
	}

	// The log's write lock keeps this process's writes to any entity in a
	// single file, which is what makes the sequence numbers in record IDs
	// meaningful. It does nothing about other processes, and it does not need
	// to: the overlap scan happens inside the store's own transaction, under
	// the store's own lock, so the split is planned against state no one else
	// can be in the middle of replacing.
	l.mu.Lock()
	defer l.mu.Unlock()

	var lastConflict error
	for attempt := 0; ; attempt++ {
		res, err := l.attemptLocked(ctx, kind, entityID, data, valid, actor, intent, o)
		if err == nil {
			return res, nil
		}
		if !errors.Is(err, ErrConflict) {
			return Result{}, err
		}
		// A conflict should not happen: the store planned this write from
		// state it held a lock over. It can still, if something outside
		// chronicle wrote an overlapping record into the same table, and the
		// honest response to that is to look again rather than to insist.
		lastConflict = err
		if attempt >= l.retries {
			return Result{}, &ConflictError{
				Reason:   "lost the race for " + kind + "/" + entityID,
				Attempts: attempt + 1,
				Err:      lastConflict,
			}
		}
		if err := sleepBackoff(ctx, attempt); err != nil {
			return Result{}, err
		}
	}
}

// backoffBase and backoffCap bound the pause between retries.
const (
	backoffBase = 200 * time.Microsecond
	backoffCap  = 10 * time.Millisecond
)

// sleepBackoff pauses before recomputing a write that lost a race.
//
// The jitter is the point rather than the delay. Two writers recomputing the
// moment they lose arrive together again and lose again, so a fixed pause
// preserves whatever phase they were already in; a random one breaks it. The
// doubling keeps a genuinely hot entity from spinning.
func sleepBackoff(ctx context.Context, attempt int) error {
	window := backoffBase << min(attempt, 6)
	if window > backoffCap {
		window = backoffCap
	}
	timer := time.NewTimer(rand.N(window) + time.Microsecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// sealForSubject encrypts a write's data under the subject's key and stamps
// the encryption markers into the write's metadata. The markers live in Meta
// so that no store needs schema for them, and they are what the read path
// recognises an encrypted record by.
func (l *Log) sealForSubject(ctx context.Context, o *writeOpts, kind, entityID string, data []byte) ([]byte, error) {
	if l.keyring == nil {
		return nil, &KeyError{Subject: o.subject, Err: ErrNoKeyring}
	}
	key, err := l.keyring.Key(ctx, o.subject)
	if err != nil {
		var ke *KeyError
		if errors.As(err, &ke) {
			return nil, err
		}
		return nil, &KeyError{Subject: o.subject, Err: err}
	}
	sealed, err := sealData(key, data, kind, entityID)
	if err != nil {
		return nil, err
	}
	if o.meta == nil {
		o.meta = make(map[string]string, 2)
	}
	o.meta[MetaSubject] = o.subject
	o.meta[MetaCipher] = CipherAESGCM1
	return sealed, nil
}

// attemptLocked is one pass of the write: hand the store a plan, let it read
// the overlapping records under its own lock, compute the split against what
// it read, and apply the result in the same transaction.
//
// The split is computed inside the planner rather than before it, which is the
// whole point. A plan built from records read in an earlier call describes a
// state that may already have moved; a plan built from records the store is
// holding a lock over cannot.
func (l *Log) attemptLocked(ctx context.Context, kind, entityID string, data []byte, valid Interval, actor Actor, intent Intent, o writeOpts) (Result, error) {
	txNow := l.tickLocked()

	// Under chaining the plan asks for every current record, not only the
	// overlapping ones: the chain tail — the record the new links hash from —
	// is the greatest current record in chronicle's total order, and it may
	// well not overlap the interval being written. The planner then narrows to
	// the genuinely overlapping records itself before splitting.
	planValid := valid
	if l.chain {
		planValid = Always()
	}

	var (
		inserts    []Record
		superseded []RecordID
	)
	plan := func(current []Record, txAt time.Time) (Write, error) {
		overlapping := current
		if l.chain {
			overlapping = overlapping[:0:0]
			for _, r := range current {
				if r.Valid().Overlaps(valid) {
					overlapping = append(overlapping, r)
				}
			}
		}

		// Record IDs lead with the transaction instant, so they are minted
		// here, against the instant the store settled on, rather than against
		// the one this log proposed.
		inserts = make([]Record, 0, 1+2*len(overlapping))
		inserts = append(inserts, Record{
			ID:        l.nextIDLocked(txAt),
			EntityID:  entityID,
			Kind:      kind,
			Data:      data,
			ValidFrom: valid.From,
			ValidTo:   valid.To,
			TxFrom:    txAt,
			Actor:     actor,
			Reason:    o.reason,
			Intent:    intent,
			Meta:      o.meta,
		})

		superseded = make([]RecordID, 0, len(overlapping))
		for _, r := range overlapping {
			superseded = append(superseded, r.ID)

			// Left remainder: the part of r that starts before the new interval.
			if r.Valid().StartsBefore(valid) {
				inserts = append(inserts, l.remainderLocked(r, Interval{From: r.ValidFrom, To: valid.From}, txAt))
			}
			// Right remainder: the part of r that outlasts the new interval. An
			// unbounded r always has one unless the new interval is unbounded too,
			// which is exactly what ExtendsBeyond encodes.
			if r.Valid().ExtendsBeyond(valid) {
				inserts = append(inserts, l.remainderLocked(r, Interval{From: valid.To, To: r.ValidTo}, txAt))
			}
		}
		if l.chain {
			chainStamp(kind, entityID, current, inserts)
		}
		return Write{Supersede: superseded, Insert: inserts}, nil
	}

	txAt, err := l.store.Apply(ctx, ApplyRequest{
		Entity: EntityRef{Kind: kind, EntityID: entityID},
		Valid:  planValid,
		TxAt:   txNow,
		Plan:   plan,
	})
	if err != nil {
		return Result{}, err
	}

	// Every write plans at least its own record, so a successful Apply that
	// left the plan's inserts empty means the store never ran the plan — a
	// contract violation that would otherwise surface as an index panic below,
	// blamed on the log rather than the store that earned it.
	if len(inserts) == 0 {
		return Result{}, fmt.Errorf("chronicle: store %T reported success but did not execute the plan (Store.Apply contract violation)", l.store)
	}

	// The store has the last word on transaction time, and the ratchet is
	// pulled forward to match so that a subsequent read's notion of "now" sits
	// after the write that just happened rather than before it.
	txAt = txAt.UTC()
	if txAt.IsZero() {
		txAt = txNow
	}
	if txAt.After(l.lastTx) {
		l.lastTx = txAt
	}

	written := cloneRecords(inserts)
	return Result{
		TxAt:       txAt,
		Written:    written,
		Superseded: superseded,
		Record:     written[0],
	}, nil
}

// remainderLocked builds the record preserving an uncovered part of a
// superseded record's valid interval.
func (l *Log) remainderLocked(r Record, valid Interval, txNow time.Time) Record {
	return Record{
		ID:        l.nextIDLocked(txNow),
		EntityID:  r.EntityID,
		Kind:      r.Kind,
		Data:      r.Data,
		ValidFrom: valid.From,
		ValidTo:   valid.To,
		TxFrom:    txNow,
		Actor:     r.Actor,
		Reason:    r.Reason,
		Intent:    IntentRemainder,
		Meta:      r.Meta,
	}
}

// tickLocked returns the transaction instant for a write, applying the
// monotonic ratchet described on [Log].
//
// The guard is "fails to advance", with no special case for a virgin ratchet:
// a clock reading that does not sit strictly after lastTx becomes lastTx plus
// one nanosecond, even when lastTx is the zero time. Exempting the first write
// would let a zero clock reading straight through, and a zero transaction
// instant is not a timestamp at all — stamped as TxFrom it reads as "always
// believed", and stamped as TxTo it reads as "still current", which is how a
// supersession quietly fails to supersede.
func (l *Log) tickLocked() time.Time {
	now := l.clock.Now().UTC()
	if !now.After(l.lastTx) {
		now = l.lastTx.Add(time.Nanosecond)
	}
	l.lastTx = now
	return now
}

// nextIDLocked mints a record ID that sorts in write order. The transaction
// instant leads so that IDs and transaction time agree; the sequence number
// separates records written at the same instant, which is every multi-record
// write, since a write and its remainders share one transaction time; and the
// node token makes the result unique across processes, which neither of the
// other two parts is.
func (l *Log) nextIDLocked(txNow time.Time) RecordID {
	l.seq++
	return RecordID(txNow.Format("20060102T150405.000000000Z") + "-" + pad(l.seq, 12) + "-" + l.node)
}

// pad renders n zero-padded to at least width digits, so that IDs minted at
// the same transaction instant sort lexicographically in write order.
func pad(n uint64, width int) string {
	s := strconv.FormatUint(n, 10)
	if len(s) >= width {
		return s
	}
	return strings.Repeat("0", width-len(s)) + s
}

func (l *Log) checkKind(kind string) error {
	if kind == "" {
		return &KindError{Err: ErrUnknownKind}
	}
	if l.kinds == nil {
		return nil
	}
	if _, ok := l.kinds[kind]; !ok {
		return &KindError{Kind: kind, Err: ErrUnknownKind}
	}
	return nil
}

// now returns the instant a read should treat as "now".
//
// It is the later of the clock and the most recent write. The ratchet can push
// transaction time ahead of a frozen or coarse clock, and a read that used the
// raw clock would then sit before the newest records and fail to see writes
// that have demonstrably already happened. Taking the maximum keeps "now"
// meaning "after everything that has been written", which is what a caller
// asking for the current state means by it.
func (l *Log) now() time.Time {
	l.mu.RLock()
	last := l.lastTx
	l.mu.RUnlock()
	now := l.clock.Now().UTC()
	if last.After(now) {
		return last
	}
	return now
}
