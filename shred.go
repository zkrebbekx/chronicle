package chronicle

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"sync"
)

// Crypto-shredding: per-subject encryption of record data, so that destroying
// a subject's key renders their historical values unrecoverable while every
// record's structure — its intervals, actor, intent, metadata — survives.
//
// The legal characterization is deliberately hedged, and the hedge is
// binding: whether key destruction constitutes erasure under GDPR Art.17
// depends on your supervisory authority's position, no DPA decision, EDPB
// guidance or court ruling accepting it was verified by the research behind
// docs/COMPLIANCE.md, and chronicle makes no compliance claim. This file
// implements a mechanism; what the mechanism satisfies is a question for your
// counsel, not for a library.

// Reserved metadata keys the shredding machinery writes. See
// [MetaReservedPrefix].
const (
	// MetaSubject names the subject whose key a record's data is encrypted
	// under. Its presence is what marks a record as encrypted.
	MetaSubject = MetaReservedPrefix + "subject"
	// MetaCipher names the encryption scheme, currently always
	// [CipherAESGCM1].
	MetaCipher = MetaReservedPrefix + "enc"
)

// CipherAESGCM1 is the record encryption scheme: AES-256-GCM with a random
// 96-bit nonce prepended to the ciphertext, and the record's kind and entity
// ID as additional authenticated data so a ciphertext cannot be replayed onto
// another entity.
const CipherAESGCM1 = "aesgcm1"

// KeySize is the size of a subject data key in bytes: 32, for AES-256.
const KeySize = 32

// Keyring stores per-subject data keys. It is the pluggable half of
// crypto-shredding: chronicle encrypts and decrypts, the keyring decides
// where key material lives and what destroying it means.
//
// The keyring's storage is the whole strength of the scheme. Shredding
// assumes destroying a key destroys every copy — a keyring whose backups
// retain destroyed keys, or which lives in the same database and the same
// backup cycle as the records it protects, undoes the destruction it
// reports. [MemKeyring] and the Postgres keyring in pgstore are provided for
// development and for deployments that accept those properties; a deployment
// that needs shredding to mean something should back this interface with a
// KMS or HSM whose key-destruction semantics it trusts.
//
// Implementations must be safe for concurrent use.
type Keyring interface {
	// Key returns the subject's data key, minting one — [KeySize] random
	// bytes — on first use. After [Keyring.DestroyKey] it fails with an error
	// wrapping [ErrKeyDestroyed]: destruction is terminal for the subject,
	// because quietly re-minting under the same identifier would make new
	// writes readable under a name the caller believes erased.
	Key(ctx context.Context, subject string) ([]byte, error)

	// DestroyKey irrevocably destroys the subject's key, rendering every
	// value encrypted under it unrecoverable. It is idempotent, and it
	// succeeds for a subject that never had a key — recording the destruction
	// so no key can be minted for that subject later — because "make this
	// subject unreadable" should not fail on the subject it cannot read.
	DestroyKey(ctx context.Context, subject string) error
}

// MemKeyring is an in-memory [Keyring], for tests and for callers who want
// the mechanism without persistent key storage. Keys vanish with the process:
// restarting is indistinguishable from having destroyed every key, which
// makes MemKeyring the wrong choice anywhere the data outlives the process.
type MemKeyring struct {
	mu        sync.Mutex
	keys      map[string][]byte
	destroyed map[string]struct{}
}

// NewMemKeyring returns an empty in-memory keyring.
func NewMemKeyring() *MemKeyring {
	return &MemKeyring{
		keys:      make(map[string][]byte),
		destroyed: make(map[string]struct{}),
	}
}

// Key implements [Keyring].
func (k *MemKeyring) Key(ctx context.Context, subject string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if subject == "" {
		return nil, &KeyError{Err: errors.New("subject is empty")}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, gone := k.destroyed[subject]; gone {
		return nil, &KeyError{Subject: subject, Err: ErrKeyDestroyed}
	}
	if key, ok := k.keys[subject]; ok {
		out := make([]byte, len(key))
		copy(out, key)
		return out, nil
	}
	key := make([]byte, KeySize)
	if _, err := cryptorand.Read(key); err != nil {
		return nil, &KeyError{Subject: subject, Err: err}
	}
	k.keys[subject] = key
	out := make([]byte, len(key))
	copy(out, key)
	return out, nil
}

// DestroyKey implements [Keyring]. The key material is zeroed before the
// reference is dropped; the destruction marker persists so the subject can
// never be re-keyed.
func (k *MemKeyring) DestroyKey(ctx context.Context, subject string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if subject == "" {
		return &KeyError{Err: errors.New("subject is empty")}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if key, ok := k.keys[subject]; ok {
		for i := range key {
			key[i] = 0
		}
		delete(k.keys, subject)
	}
	k.destroyed[subject] = struct{}{}
	return nil
}

// Compile-time assertion.
var _ Keyring = (*MemKeyring)(nil)

// WithKeyring gives the log a [Keyring], enabling [WithSubject] on writes and
// transparent decryption on [Log.Get] and [Log.Diff]. Without one, writes
// naming a subject and reads of encrypted records fail with [ErrNoKeyring].
func WithKeyring(k Keyring) Option {
	return func(l *Log) {
		if k != nil {
			l.keyring = k
		}
	}
}

// WithSubject encrypts the write's data under the subject's key from the
// log's [Keyring], so that [Keyring.DestroyKey] later renders the value —
// this record and every remainder that inherits its data — unrecoverable.
//
// The stored record's Data is ciphertext, its metadata carries [MetaSubject]
// and [MetaCipher], and everything else about it is unchanged: intervals,
// actor, intent and caller metadata stay plaintext, which is what "preserving
// the record structure" means and is the entire trade the mechanism offers.
// Choose subject identifiers accordingly — the subject string itself is
// stored in clear and survives shredding, so it should be a pseudonymous
// reference, not the personal data it protects.
//
// Under [WithChaining], the hash covers the ciphertext as stored, so
// destroying a key changes no hash and breaks no chain.
func WithSubject(subject string) WriteOption {
	return func(o *writeOpts) { o.subject = subject }
}

// aad binds a ciphertext to its entity, so a value encrypted for one record
// cannot be replayed onto another entity's history and still authenticate.
// It cannot include the record ID: remainders legitimately carry a superseded
// record's ciphertext under a new ID.
func aad(kind, entityID string) []byte {
	return []byte("chronicle/" + CipherAESGCM1 + "\x1f" + kind + "\x1f" + entityID)
}

const gcmNonceSize = 12

// sealData encrypts plaintext under key for one entity: nonce, then
// AES-256-GCM ciphertext and tag.
func sealData(key, plaintext []byte, kind, entityID string) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("chronicle: seal: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("chronicle: seal: %w", err)
	}
	nonce := make([]byte, gcmNonceSize, gcmNonceSize+len(plaintext)+gcm.Overhead())
	if _, err := cryptorand.Read(nonce); err != nil {
		return nil, fmt.Errorf("chronicle: seal: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, aad(kind, entityID)), nil
}

// openData reverses [sealData].
func openData(key, sealed []byte, kind, entityID string) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("chronicle: open: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("chronicle: open: %w", err)
	}
	if len(sealed) < gcmNonceSize+gcm.Overhead() {
		return nil, errors.New("chronicle: open: sealed data shorter than a nonce and tag")
	}
	return gcm.Open(nil, sealed[:gcmNonceSize], sealed[gcmNonceSize:], aad(kind, entityID))
}

// Decrypt returns the record with its data decrypted, when the record is
// encrypted and the log's keyring still holds the subject's key. A record
// that is not encrypted comes back unchanged.
//
// [Log.Get] and [Log.Diff] decrypt implicitly; [Log.History], [Log.Timeline]
// and [Log.Query] deliberately do not — they are views of the log as stored,
// they must keep working after a subject is shredded, and a page that fails
// because one record on it is unreadable would make shredding break the audit
// trail it is meant to leave intact. Decrypt is the explicit step for callers
// walking those views who want values back.
//
// Failure is loud by design: a destroyed key is a [*ShredError] wrapping
// [ErrShredded], a missing keyring is [ErrNoKeyring], and a ciphertext that
// fails authentication — tampered, or its key destroyed and re-minted by a
// keyring that does not honour terminal destruction — is a [*ShredError]
// too. No path returns ciphertext or garbage where plaintext was asked for.
func (l *Log) Decrypt(ctx context.Context, r Record) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	out, err := l.decryptRecord(ctx, &r)
	if err != nil {
		return Record{}, err
	}
	return *out, nil
}

// decryptRecord decrypts a record read from the store, if it is marked
// encrypted. It returns the input pointer untouched for plaintext records,
// and a decrypted clone for encrypted ones.
func (l *Log) decryptRecord(ctx context.Context, r *Record) (*Record, error) {
	if r == nil {
		return nil, nil
	}
	subject, encrypted := r.Meta[MetaSubject]
	if !encrypted {
		return r, nil
	}
	if scheme := r.Meta[MetaCipher]; scheme != CipherAESGCM1 {
		return nil, &ShredError{
			Subject:  subject,
			RecordID: r.ID,
			Reason:   fmt.Sprintf("unrecognised encryption scheme %q", scheme),
		}
	}
	if l.keyring == nil {
		return nil, &KeyError{Subject: subject, Err: ErrNoKeyring}
	}
	key, err := l.keyring.Key(ctx, subject)
	if err != nil {
		if errors.Is(err, ErrKeyDestroyed) {
			return nil, &ShredError{Subject: subject, RecordID: r.ID, Err: err}
		}
		return nil, &KeyError{Subject: subject, Err: err}
	}
	plaintext, err := openData(key, r.Data, r.Kind, r.EntityID)
	if err != nil {
		return nil, &ShredError{
			Subject:  subject,
			RecordID: r.ID,
			Reason:   "ciphertext failed to authenticate — it was altered, or the keyring re-minted a destroyed key",
			Err:      err,
		}
	}
	out := r.Clone()
	out.Data = plaintext
	return &out, nil
}
