package chronicle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"maps"
	"slices"
	"strings"
	"time"
)

// Reserved metadata keys. chronicle keeps its own bookkeeping in [Record.Meta]
// under the [MetaReservedPrefix] namespace, and every write path rejects
// caller-supplied metadata using that prefix with [ErrReservedMeta] — a caller
// who could write these keys could forge exactly what they exist to attest.
const (
	// MetaReservedPrefix is the namespace of metadata keys chronicle writes
	// itself. Caller metadata must not use it.
	MetaReservedPrefix = "chronicle:"

	// MetaChain carries a record's hash-chain value, written when the log was
	// built [WithChaining]. The value is a chain format version prefix and a
	// hex digest, currently "v1:" followed by 64 hex characters. Treat it as
	// opaque; [Log.Verify] is how it is checked.
	MetaChain = MetaReservedPrefix + "chain"
)

// chainV1 is the current chain format version: the leading byte of every
// canonical serialization, and the "v1" of the stored token. Any future change
// to the canonical form — a field added to [Record], a different time
// encoding — must bump it, so that a verifier meets an explicit unknown
// version instead of a silent mismatch against records hashed under rules it
// no longer follows.
const (
	chainV1      byte = 0x01
	chainV1Token      = "v1:"
)

// WithChaining makes every write extend a per-entity hash chain, giving the
// log tamper evidence. Off by default.
//
// Each record chronicle inserts — the caller's record and every remainder —
// gets a [MetaChain] metadata value: a SHA-256 over the previous chain value
// and a canonical serialization of the record's own immutable fields. The
// chain runs per (kind, entity) in chronicle's total order, and [Log.Verify]
// recomputes it to detect records that were modified, reordered, removed
// without a tombstone, or inserted from outside the chain.
//
// # The honest threat model
//
// A hash chain detects retrospective edits by someone who does not control
// the chain head. It does nothing against an administrator who owns the
// database: they can alter a record and recompute every hash after it, heads
// included, and Verify will pass. Only anchoring chain heads outside the
// database changes that — read a head with [Log.ChainHead] and lodge it
// somewhere the database administrator cannot reach — and chronicle ships no
// anchoring, so it claims nothing anchoring would be needed for. No
// regulation in the corpus behind docs/COMPLIANCE.md requires any of this;
// chaining is offered because it is useful against the threat it actually
// addresses, not because compliance demands it.
//
// # What the hash does and does not cover
//
// The hash covers the record's immutable fields: ID, kind, entity, data,
// valid interval, transaction start, actor, reason, intent, and metadata
// (minus the chain value itself). It cannot cover TxTo, because TxTo is
// written later, when the record is superseded — the one mutation chronicle's
// model permits. Verify compensates by checking every superseded record's
// TxTo against the transaction starts of later chained writes, which pins it
// to the set of instants the chain vouches for, though not to one of them.
//
// Timestamps are canonicalized at microsecond resolution — the storage
// contract's floor, set by Postgres timestamptz — so a change below one
// microsecond in a caller-supplied valid time is invisible to the chain, just
// as it is invisible to a round trip through the Postgres adapter.
//
// Data encrypted for a subject (see [WithSubject]) is hashed as stored — the
// ciphertext — so destroying a subject's key afterwards changes no hash and
// breaks no chain.
//
// # Rules of use
//
// Chaining is per-writer, and a chain is only as complete as its writers are
// consistent: enable it on every Log that writes an entity, or on none.
// Records written without chaining before the chain began are tolerated as an
// unchained prefix; an unchained record after the chain began reads as an
// insertion and fails Verify — which is the correct reading, since out-of-band
// writes are indistinguishable from the tampering the chain exists to catch.
func WithChaining() Option {
	return func(l *Log) { l.chain = true }
}

// ---------------------------------------------------------------------------
// canonical serialization and hashing
// ---------------------------------------------------------------------------

// canonicalRecord renders the record's immutable fields deterministically:
// the chain format version byte, then every field length-prefixed in a fixed
// order, metadata sorted by key with the chain value itself excluded.
//
// Length prefixes rather than separators, so that no arrangement of field
// contents can collide with another record's encoding.
func canonicalRecord(r Record) []byte {
	buf := make([]byte, 0, 128+len(r.Data))
	buf = append(buf, chainV1)
	appendField := func(s string) {
		buf = binary.AppendUvarint(buf, uint64(len(s)))
		buf = append(buf, s...)
	}
	appendField(string(r.ID))
	appendField(r.Kind)
	appendField(r.EntityID)
	appendField(string(r.Data))
	appendField(canonicalTime(r.ValidFrom))
	appendField(canonicalTime(r.ValidTo))
	appendField(canonicalTime(r.TxFrom))
	appendField(r.Actor.ID)
	appendField(r.Actor.Type)
	appendField(r.Actor.Name)
	appendField(r.Reason)
	buf = append(buf, byte(r.Intent))

	keys := make([]string, 0, len(r.Meta))
	for k := range r.Meta {
		if k != MetaChain {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	buf = binary.AppendUvarint(buf, uint64(len(keys)))
	for _, k := range keys {
		appendField(k)
		appendField(r.Meta[k])
	}
	return buf
}

// canonicalTime renders an instant at microsecond resolution in UTC, and the
// zero time as the empty string. Microseconds, not nanoseconds, because the
// storage contract's resolution floor is Postgres timestamptz: a hash over
// digits a store is allowed to drop would fail verification against exactly
// the records it was meant to protect.
func canonicalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z")
}

// chainGenesis is the chain value before the first record: a digest bound to
// the entity, so that even a whole chain transplanted from another entity
// fails at its first link.
func chainGenesis(kind, entityID string) []byte {
	sum := sha256.Sum256([]byte("chronicle/chain/v1\x1f" + kind + "\x1f" + entityID))
	return sum[:]
}

// chainNext computes the chain value of a record given its predecessor's.
func chainNext(prev []byte, r Record) []byte {
	h := sha256.New()
	h.Write(prev)
	h.Write(canonicalRecord(r))
	return h.Sum(nil)
}

// chainToken renders a chain value as it is stored in [MetaChain].
func chainToken(hash []byte) string { return chainV1Token + hex.EncodeToString(hash) }

// parseChainToken decodes a stored chain value. ok is false for an empty,
// unversioned, unrecognised or malformed token — all of which Verify treats
// as divergence rather than guessing.
func parseChainToken(token string) (hash []byte, ok bool) {
	rest, found := strings.CutPrefix(token, chainV1Token)
	if !found {
		return nil, false
	}
	hash, err := hex.DecodeString(rest)
	if err != nil || len(hash) != sha256.Size {
		return nil, false
	}
	return hash, true
}

// chainStamp links a write's inserted records onto the entity's chain,
// in place.
//
// current is every current record for the entity — the store hands the whole
// set over when chaining is on — and the chain tail is the greatest of them
// in chronicle's total order. That record is always the newest write's,
// because every Log write inserts at least one record and only later writes
// could have superseded it; deleted records cannot be the tail because only
// superseded records are ever deleted. A tail without a chain value means the
// entity's history predates chaining, and the chain starts here, from
// genesis.
//
// The inserted records are hashed in the order they will occupy in the chain,
// which is chronicle's total order, not insertion order: a left remainder has
// an earlier valid start than the record whose write produced it.
func chainStamp(kind, entityID string, current, inserts []Record) {
	prev := chainGenesis(kind, entityID)
	var tail *Record
	for i := range current {
		if tail == nil || CompareRecords(current[i], *tail) > 0 {
			tail = &current[i]
		}
	}
	if tail != nil {
		if hash, ok := parseChainToken(tail.Meta[MetaChain]); ok {
			prev = hash
		}
	}

	order := make([]*Record, len(inserts))
	for i := range inserts {
		order[i] = &inserts[i]
	}
	slices.SortFunc(order, func(a, b *Record) int { return CompareRecords(*a, *b) })

	for _, r := range order {
		// Remainders arrive carrying the superseded record's metadata, chain
		// value included; each record gets a fresh map with its own link.
		meta := make(map[string]string, len(r.Meta)+1)
		maps.Copy(meta, r.Meta)
		delete(meta, MetaChain)
		if len(meta) > 0 {
			r.Meta = meta
		} else {
			r.Meta = map[string]string{}
		}
		prev = chainNext(prev, *r)
		r.Meta[MetaChain] = chainToken(prev)
	}
}

// ---------------------------------------------------------------------------
// verification
// ---------------------------------------------------------------------------

// VerifyReport is the result of [Log.Verify]: what the chain covered, and the
// first point at which it stopped adding up, if any.
type VerifyReport struct {
	// Kind and EntityID identify the verified entity.
	Kind, EntityID string
	// ChainedRecords is how many surviving records the chain vouches for.
	ChainedRecords int
	// Tombstones is how many destroyed records the chain passed over on their
	// retained hashes. A tombstone attests that a record with that chain value
	// stood there — not what it said, and not whether destroying it was
	// authorised. See [Tombstone].
	Tombstones int
	// UnchainedPrefix is how many records predate the chain — written before
	// chaining was enabled for this entity. They are outside the chain's
	// protection and counted so that a report never silently understates what
	// it did not check.
	UnchainedPrefix int
	// Head is the chain's final value, set only when the chain is intact. It
	// is the value to anchor externally; see [Log.ChainHead].
	Head []byte
	// Divergence is the first point at which the chain failed to verify, nil
	// when it is intact.
	Divergence *Divergence
}

// Intact reports whether the chain verified end to end.
func (r VerifyReport) Intact() bool { return r.Divergence == nil }

// Divergence names the first chain entry that failed verification.
type Divergence struct {
	// RecordID is the record — or, for a missing tombstone hash, the
	// tombstone — at which the chain stopped adding up.
	RecordID RecordID
	// Position is the entry's index in chain order, counting records and
	// tombstones together from zero.
	Position int
	// Reason says what failed.
	Reason string
}

// chainEntry is one position in an entity's chain: a surviving record or a
// tombstone standing in for a destroyed one.
type chainEntry struct {
	rec  *Record
	tomb *Tombstone
}

func (e chainEntry) id() RecordID {
	if e.rec != nil {
		return e.rec.ID
	}
	return e.tomb.RecordID
}

func (e chainEntry) sortKey() (time.Time, time.Time, RecordID) {
	if e.rec != nil {
		return e.rec.TxFrom, e.rec.ValidFrom, e.rec.ID
	}
	return e.tomb.TxFrom, e.tomb.ValidFrom, e.tomb.RecordID
}

func compareEntries(a, b chainEntry) int {
	atx, av, aid := a.sortKey()
	btx, bv, bid := b.sortKey()
	if c := atx.Compare(btx); c != 0 {
		return c
	}
	if c := compareStarts(av, bv); c != 0 {
		return c
	}
	return strings.Compare(string(aid), string(bid))
}

// loadChain assembles an entity's chain entries — every surviving record and,
// where the store can report them, every tombstone — in chain order.
func (l *Log) loadChain(ctx context.Context, kind, entityID string) ([]chainEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := l.checkKind(kind); err != nil {
		return nil, err
	}
	if entityID == "" {
		return nil, ErrMissingEntityID
	}

	recs, _, err := l.store.Query(ctx, Query{Kind: kind, EntityID: entityID})
	if err != nil {
		return nil, &ChainError{Kind: kind, EntityID: entityID, Err: err}
	}
	var tombs []Tombstone
	if d, ok := l.store.(Deleter); ok {
		tombs, err = d.Tombstones(ctx, kind, entityID)
		if err != nil {
			return nil, &ChainError{Kind: kind, EntityID: entityID, Err: err}
		}
	}

	entries := make([]chainEntry, 0, len(recs)+len(tombs))
	for i := range recs {
		entries = append(entries, chainEntry{rec: &recs[i]})
	}
	for i := range tombs {
		entries = append(entries, chainEntry{tomb: &tombs[i]})
	}
	slices.SortFunc(entries, compareEntries)
	return entries, nil
}

// Verify recomputes an entity's hash chain and reports the first divergence,
// if any. A divergence is reported in the [VerifyReport], not as an error;
// the error return is for the entity being unreadable, or having no chain at
// all — [ErrNoChain] — which must never be mistaken for a chain that
// verified.
//
// Verify detects a chained record whose content differs from what was hashed,
// a chained record moved relative to its neighbours, a record removed without
// a tombstone, and a record inserted after the chain began without being part
// of it. It cannot check the content standing behind a tombstone — that
// content is destroyed — and within a run of consecutive tombstones only the
// last one's hash is constrained by a surviving successor; see [Tombstone]
// for exactly how little a tombstone proves. It also cannot detect anything
// done by an actor who recomputed the entire chain, per the threat model on
// [WithChaining].
//
// Verify does not require the log to have been built [WithChaining]: an
// auditor process that only reads can verify what the writers chained.
func (l *Log) Verify(ctx context.Context, kind, entityID string) (VerifyReport, error) {
	entries, err := l.loadChain(ctx, kind, entityID)
	if err != nil {
		return VerifyReport{}, err
	}

	report := VerifyReport{Kind: kind, EntityID: entityID}
	prev := chainGenesis(kind, entityID)
	started := false
	diverge := func(pos int, id RecordID, reason string) {
		report.Divergence = &Divergence{RecordID: id, Position: pos, Reason: reason}
	}

	// txStarts collects the transaction instants the chain vouches for, and
	// closed the chained records whose TxTo must be found among them.
	txStarts := make(map[int64]struct{})
	var closed []*Record

	for pos, e := range entries {
		if e.tomb != nil {
			hash, ok := parseChainToken(e.tomb.ChainHash)
			if !ok {
				diverge(pos, e.id(), "tombstone carries no usable chain hash, so the chain cannot cross the gap it stands in")
				break
			}
			// A tombstone's hash is taken on trust: the content it summarised
			// is destroyed, so only the next surviving record constrains it.
			prev = hash
			started = true
			report.Tombstones++
			txStarts[e.tomb.TxFrom.UnixNano()] = struct{}{}
			continue
		}

		r := e.rec
		token, chained := r.Meta[MetaChain]
		if !chained {
			if started {
				diverge(pos, r.ID, "record is not covered by the chain — written or inserted outside it after the chain began")
				break
			}
			report.UnchainedPrefix++
			continue
		}
		stored, ok := parseChainToken(token)
		if !ok {
			diverge(pos, r.ID, "chain value is malformed or of an unrecognised format version")
			break
		}
		computed := chainNext(prev, *r)
		if !bytes.Equal(computed, stored) {
			diverge(pos, r.ID, "stored chain value does not match the recomputed one — the record's content, its position, or a predecessor differs from what was written")
			break
		}
		prev = computed
		started = true
		report.ChainedRecords++
		txStarts[r.TxFrom.UnixNano()] = struct{}{}
		if !r.IsCurrent() {
			closed = append(closed, r)
		}
	}

	if !started && report.Divergence == nil {
		return VerifyReport{}, &ChainError{Kind: kind, EntityID: entityID, Err: ErrNoChain}
	}

	// TxTo is the one field the hash cannot cover, because it is written after
	// the hash, at supersession. This pins it to the set of instants later
	// chained writes vouch for — not to the right one among them, which is the
	// residual, documented gap.
	if report.Divergence == nil {
		for _, r := range closed {
			if _, ok := txStarts[r.TxTo.UnixNano()]; !ok || !r.TxTo.After(r.TxFrom) {
				report.Divergence = &Divergence{
					RecordID: r.ID,
					Reason:   "superseded at an instant matching no later chained write — TxTo has been altered, or the write that closed it bypassed the chain",
				}
				break
			}
		}
	}

	if report.Divergence == nil {
		report.Head = prev
	}
	return report, nil
}

// ChainHead returns the entity's current chain head: the chain value of the
// last entry — record or tombstone — in chain order. It reads the stored
// value without verifying the chain behind it; anchoring and verification are
// separate acts, and an anchor of what the database currently claims is
// exactly what makes a later recomputed chain provable.
//
// Anchoring is the caller's, and chronicle is explicit about the division: a
// head lodged somewhere the database administrator cannot reach — a ledger, a
// timestamping service, a different trust domain's storage — upgrades the
// chain from "detects editors who do not control the head" to "detects
// editors who do not control the anchor". chronicle ships no anchoring.
//
// It reports [ErrNoChain] when the entity has no chained entries.
func (l *Log) ChainHead(ctx context.Context, kind, entityID string) ([]byte, error) {
	entries, err := l.loadChain(ctx, kind, entityID)
	if err != nil {
		return nil, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		var token string
		if e := entries[i]; e.tomb != nil {
			token = e.tomb.ChainHash
		} else {
			token = e.rec.Meta[MetaChain]
		}
		if hash, ok := parseChainToken(token); ok {
			return hash, nil
		}
	}
	return nil, &ChainError{Kind: kind, EntityID: entityID, Err: ErrNoChain}
}
