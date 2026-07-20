package chronicle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func newShredLog(t *testing.T) (*Log, *MemStore, *MemKeyring, *FixedClock) {
	t.Helper()
	keyring := NewMemKeyring()
	clock := NewFixedClock(t0)
	store := NewMemStore()
	return NewLog(store, WithClock(clock), WithKeyring(keyring)), store, keyring, clock
}

func TestShredding(t *testing.T) {
	ctx := context.Background()
	plaintext := `{"name":"Alice","salary":50000}`

	t.Run("given a write for a subject", func(t *testing.T) {
		l, store, _, clock := newShredLog(t)
		clock.Advance(time.Second)
		res, err := l.Put(ctx, employee, "e1", []byte(plaintext), t1, time.Time{}, alice,
			WithSubject("subj-alice"), WithMetaValue("ticket", "HR-1"))
		if err != nil {
			t.Fatalf("Put failed: %v", err)
		}

		t.Run("when the store's own contents are examined", func(t *testing.T) {
			raw := byIDStore(t, store, res.Record.ID)
			t.Run("then the stored data is ciphertext, not the plaintext", func(t *testing.T) {
				if strings.Contains(string(raw.Data), "Alice") {
					t.Fatal("plaintext reached the store")
				}
			})
			t.Run("then the record is marked with subject and scheme", func(t *testing.T) {
				if raw.Meta[MetaSubject] != "subj-alice" || raw.Meta[MetaCipher] != CipherAESGCM1 {
					t.Fatalf("markers = %v; want subject and scheme", raw.Meta)
				}
			})
			t.Run("then caller metadata rides alongside in clear", func(t *testing.T) {
				if raw.Meta["ticket"] != "HR-1" {
					t.Fatalf("meta = %v; want the caller's ticket preserved", raw.Meta)
				}
			})
		})

		t.Run("when it is read back through the log", func(t *testing.T) {
			t.Run("then Get returns the plaintext", func(t *testing.T) {
				got, err := l.Get(ctx, employee, "e1", As{ValidAt: t2})
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if string(got.Data) != plaintext {
					t.Fatalf("Get = %s; want the plaintext", got.Data)
				}
			})
			t.Run("then History returns the record as stored, ciphertext and all", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "e1")
				if err != nil {
					t.Fatalf("History failed: %v", err)
				}
				if strings.Contains(string(recs[0].Data), "Alice") {
					t.Fatal("History decrypted implicitly; it must return the log as stored")
				}
			})
			t.Run("then Decrypt turns a History record into plaintext explicitly", func(t *testing.T) {
				recs, _ := l.History(ctx, employee, "e1")
				dec, err := l.Decrypt(ctx, recs[0])
				if err != nil {
					t.Fatalf("Decrypt failed: %v", err)
				}
				if string(dec.Data) != plaintext {
					t.Fatalf("Decrypt = %s; want the plaintext", dec.Data)
				}
			})
			t.Run("then Decrypt of an unencrypted record is a pass-through", func(t *testing.T) {
				rec := Record{ID: "r", Data: []byte("plain")}
				dec, err := l.Decrypt(ctx, rec)
				if err != nil || string(dec.Data) != "plain" {
					t.Fatalf("Decrypt = (%s, %v); want the record unchanged", dec.Data, err)
				}
			})
		})

		t.Run("when the subject's data changes and is diffed", func(t *testing.T) {
			clock.Advance(time.Second)
			second, err := l.Correct(ctx, employee, "e1", []byte(`{"name":"Alice","salary":60000}`),
				t1, time.Time{}, bob, WithSubject("subj-alice"))
			if err != nil {
				t.Fatalf("Correct failed: %v", err)
			}
			delta, err := l.Diff(ctx, employee, "e1",
				As{ValidAt: t2, TxAt: res.TxAt}, As{ValidAt: t2, TxAt: second.TxAt})
			t.Run("then the diff is over plaintext values", func(t *testing.T) {
				if err != nil {
					t.Fatalf("Diff failed: %v", err)
				}
				if len(delta.Changes) != 1 || delta.Changes[0].Path != "/salary" {
					t.Fatalf("changes = %+v; want one at /salary", delta.Changes)
				}
			})
		})
	})

	t.Run("given a subject whose interval is later split", func(t *testing.T) {
		l, _, _, clock := newShredLog(t)
		clock.Advance(time.Second)
		if _, err := l.Put(ctx, employee, "e1", []byte(plaintext), t1, t5, alice,
			WithSubject("subj-alice")); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		clock.Advance(time.Second)
		if _, err := l.Put(ctx, employee, "e1", []byte(`{}`), t2, t3, bob); err != nil {
			t.Fatalf("Put failed: %v", err)
		}

		t.Run("when the remainder is read", func(t *testing.T) {
			got, err := l.Get(ctx, employee, "e1", As{ValidAt: t4})
			t.Run("then it decrypts like its source record", func(t *testing.T) {
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if string(got.Data) != plaintext || got.Intent != IntentRemainder {
					t.Fatalf("Get = (%s, %s); want the decrypted remainder", got.Data, got.Intent)
				}
			})
		})
	})

	t.Run("given the subject's key is destroyed", func(t *testing.T) {
		l, _, keyring, clock := newShredLog(t)
		clock.Advance(time.Second)
		if _, err := l.Put(ctx, employee, "e1", []byte(plaintext), t1, time.Time{}, alice,
			WithSubject("subj-alice")); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		if err := keyring.DestroyKey(ctx, "subj-alice"); err != nil {
			t.Fatalf("DestroyKey failed: %v", err)
		}

		t.Run("when the record is read", func(t *testing.T) {
			_, err := l.Get(ctx, employee, "e1", As{ValidAt: t2})
			t.Run("then Get fails with ErrShredded, naming subject and record", func(t *testing.T) {
				if !errors.Is(err, ErrShredded) {
					t.Fatalf("Get = %v; want ErrShredded", err)
				}
				var se *ShredError
				if !errors.As(err, &se) || se.Subject != "subj-alice" || se.RecordID == "" {
					t.Fatalf("error = %v; want a *ShredError with coordinates", err)
				}
				if !errors.Is(err, ErrKeyDestroyed) {
					t.Fatalf("error = %v; want the keyring's ErrKeyDestroyed reachable", err)
				}
			})
			t.Run("then Diff fails the same way rather than reporting no changes", func(t *testing.T) {
				_, err := l.Diff(ctx, employee, "e1", As{ValidAt: t2}, As{ValidAt: t3})
				if !errors.Is(err, ErrShredded) {
					t.Fatalf("Diff = %v; want ErrShredded", err)
				}
			})
			t.Run("then History still shows the record's structure", func(t *testing.T) {
				recs, err := l.History(ctx, employee, "e1")
				if err != nil || len(recs) != 1 {
					t.Fatalf("History = (%d, %v); shredding must not break the audit trail", len(recs), err)
				}
				r := recs[0]
				if r.Actor.ID != alice.ID || r.Meta[MetaSubject] != "subj-alice" || !r.ValidFrom.Equal(t1) {
					t.Fatalf("record structure = %+v; want it intact", r)
				}
			})
			t.Run("then Decrypt fails loudly too", func(t *testing.T) {
				recs, _ := l.History(ctx, employee, "e1")
				if _, err := l.Decrypt(ctx, recs[0]); !errors.Is(err, ErrShredded) {
					t.Fatalf("Decrypt = %v; want ErrShredded", err)
				}
			})
		})

		t.Run("when a new write names the destroyed subject", func(t *testing.T) {
			clock.Advance(time.Second)
			_, err := l.Put(ctx, employee, "e2", []byte(plaintext), t1, time.Time{}, alice,
				WithSubject("subj-alice"))
			t.Run("then it fails rather than quietly re-keying", func(t *testing.T) {
				if !errors.Is(err, ErrKeyDestroyed) {
					t.Fatalf("Put = %v; want ErrKeyDestroyed", err)
				}
			})
		})
	})

	t.Run("given tampered or mismatched ciphertext", func(t *testing.T) {
		l, store, _, clock := newShredLog(t)
		clock.Advance(time.Second)
		res, err := l.Put(ctx, employee, "e1", []byte(plaintext), t1, time.Time{}, alice,
			WithSubject("subj-alice"))
		if err != nil {
			t.Fatalf("Put failed: %v", err)
		}

		t.Run("when the ciphertext is flipped in storage", func(t *testing.T) {
			tampered := &tamperQuery{MemStore: store, fn: func(r *Record) {
				if r.ID == res.Record.ID && len(r.Data) > 0 {
					r.Data[len(r.Data)-1] ^= 0xff
				}
			}}
			// Get goes through Store.Get, so tamper via Decrypt over Query.
			vl := NewLog(tampered, WithKeyring(mustKeyringOf(l)))
			recs, _, err := tampered.Query(ctx, Query{})
			if err != nil || len(recs) != 1 {
				t.Fatalf("Query = (%d, %v)", len(recs), err)
			}
			_, err = vl.Decrypt(ctx, recs[0])
			t.Run("then decryption fails as shredded, never returning garbage", func(t *testing.T) {
				if !errors.Is(err, ErrShredded) {
					t.Fatalf("Decrypt = %v; want ErrShredded", err)
				}
				var se *ShredError
				if !errors.As(err, &se) || !strings.Contains(se.Reason, "failed to authenticate") {
					t.Fatalf("error = %v; want the authentication reason", err)
				}
			})
		})

		t.Run("when the ciphertext is replayed onto another entity", func(t *testing.T) {
			raw := byIDStore(t, store, res.Record.ID)
			moved := raw.Clone()
			moved.EntityID = "e2"
			_, err := l.Decrypt(ctx, moved)
			t.Run("then the entity binding rejects it", func(t *testing.T) {
				if !errors.Is(err, ErrShredded) {
					t.Fatalf("Decrypt = %v; want ErrShredded — the AAD binds ciphertext to its entity", err)
				}
			})
		})

		t.Run("when the scheme marker is unrecognised", func(t *testing.T) {
			raw := byIDStore(t, store, res.Record.ID)
			odd := raw.Clone()
			odd.Meta[MetaCipher] = "rot13"
			_, err := l.Decrypt(ctx, odd)
			t.Run("then it fails naming the scheme", func(t *testing.T) {
				var se *ShredError
				if !errors.As(err, &se) || !strings.Contains(se.Reason, "rot13") {
					t.Fatalf("Decrypt = %v; want a *ShredError naming the scheme", err)
				}
			})
		})

		t.Run("when the ciphertext is shorter than a nonce and tag", func(t *testing.T) {
			raw := byIDStore(t, store, res.Record.ID)
			short := raw.Clone()
			short.Data = short.Data[:4]
			_, err := l.Decrypt(ctx, short)
			t.Run("then it fails cleanly", func(t *testing.T) {
				if !errors.Is(err, ErrShredded) {
					t.Fatalf("Decrypt = %v; want ErrShredded", err)
				}
			})
		})
	})

	t.Run("given a log with no keyring", func(t *testing.T) {
		bare, store, clock := newTestLog(t)
		t.Run("when a write names a subject", func(t *testing.T) {
			clock.Advance(time.Second)
			_, err := bare.Put(ctx, employee, "e1", []byte(plaintext), t1, time.Time{}, alice,
				WithSubject("subj-alice"))
			t.Run("then it fails with ErrNoKeyring", func(t *testing.T) {
				if !errors.Is(err, ErrNoKeyring) {
					t.Fatalf("Put = %v; want ErrNoKeyring", err)
				}
			})
		})
		t.Run("when it reads a record another log encrypted", func(t *testing.T) {
			shredded, _, _, sclock := newShredLog(t)
			sclock.Advance(time.Second)
			if _, err := shredded.Put(ctx, employee, "e9", []byte(plaintext), t1, time.Time{}, alice,
				WithSubject("s")); err != nil {
				t.Fatalf("Put failed: %v", err)
			}
			_ = store
			t.Run("then Get fails with ErrNoKeyring rather than returning ciphertext", func(t *testing.T) {
				plain := NewLog(storeOf(shredded))
				if _, err := plain.Get(ctx, employee, "e9", Now()); !errors.Is(err, ErrNoKeyring) {
					t.Fatalf("Get = %v; want ErrNoKeyring", err)
				}
			})
		})
	})

	t.Run("given caller metadata in the reserved namespace", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Advance(time.Second)
		t.Run("when a write tries to smuggle a chronicle: key", func(t *testing.T) {
			_, err := l.Put(ctx, employee, "e1", []byte(`{}`), t1, time.Time{}, alice,
				WithMetaValue(MetaChain, "v1:feedface"))
			t.Run("then it is rejected", func(t *testing.T) {
				if !errors.Is(err, ErrReservedMeta) {
					t.Fatalf("Put = %v; want ErrReservedMeta — a caller who can write chain "+
						"metadata can forge what it attests", err)
				}
			})
		})
	})

	t.Run("given a chained and encrypted entity", func(t *testing.T) {
		keyring := NewMemKeyring()
		clock := NewFixedClock(t0)
		store := NewMemStore()
		l := NewLog(store, WithClock(clock), WithKeyring(keyring), WithChaining())
		clock.Advance(time.Second)
		if _, err := l.Put(ctx, employee, "e1", []byte(plaintext), t1, t2, alice,
			WithSubject("subj-alice")); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		clock.Advance(time.Second)
		if _, err := l.Put(ctx, employee, "e1", []byte(`{}`), t2, t3, bob); err != nil {
			t.Fatalf("Put failed: %v", err)
		}

		t.Run("when the key is destroyed", func(t *testing.T) {
			before := verify(t, store, "e1")
			if !before.Intact() {
				t.Fatalf("chain broken before destruction: %+v", before.Divergence)
			}
			if err := keyring.DestroyKey(ctx, "subj-alice"); err != nil {
				t.Fatalf("DestroyKey failed: %v", err)
			}
			t.Run("then the chain still verifies — the hash covers the ciphertext", func(t *testing.T) {
				after := verify(t, store, "e1")
				if !after.Intact() {
					t.Fatalf("divergence = %+v; shredding must not break tamper evidence", after.Divergence)
				}
				if string(after.Head) != string(before.Head) {
					t.Fatal("destroying a key changed the chain head")
				}
			})
		})
	})
}

func TestSealPrimitives(t *testing.T) {
	t.Run("given a key of the wrong size", func(t *testing.T) {
		short := []byte("too-short")
		t.Run("when sealing and opening", func(t *testing.T) {
			t.Run("then both refuse", func(t *testing.T) {
				if _, err := sealData(short, []byte("x"), "k", "e"); err == nil {
					t.Fatal("sealData accepted a short key")
				}
				if _, err := openData(short, make([]byte, 64), "k", "e"); err == nil {
					t.Fatal("openData accepted a short key")
				}
			})
		})
	})

	t.Run("given a cancelled context", func(t *testing.T) {
		l, _, _, _ := newShredLog(t)
		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		t.Run("when Decrypt is called", func(t *testing.T) {
			t.Run("then it reports the context error", func(t *testing.T) {
				if _, err := l.Decrypt(cctx, Record{}); !errors.Is(err, context.Canceled) {
					t.Fatalf("Decrypt = %v; want context.Canceled", err)
				}
			})
		})
	})
}

func TestMemKeyring(t *testing.T) {
	ctx := context.Background()

	t.Run("given a fresh keyring", func(t *testing.T) {
		k := NewMemKeyring()

		t.Run("when a subject's key is asked for twice", func(t *testing.T) {
			first, err := k.Key(ctx, "s1")
			if err != nil {
				t.Fatalf("Key failed: %v", err)
			}
			second, err := k.Key(ctx, "s1")
			if err != nil {
				t.Fatalf("Key failed: %v", err)
			}
			t.Run("then it is minted once, at the right size, and stable", func(t *testing.T) {
				if len(first) != KeySize {
					t.Fatalf("key length = %d; want %d", len(first), KeySize)
				}
				if string(first) != string(second) {
					t.Fatal("the same subject got two different keys")
				}
			})
			t.Run("then different subjects get different keys", func(t *testing.T) {
				other, err := k.Key(ctx, "s2")
				if err != nil {
					t.Fatalf("Key failed: %v", err)
				}
				if string(other) == string(first) {
					t.Fatal("two subjects share a key; destroying one would shred the other")
				}
			})
			t.Run("then mutating a returned key does not corrupt the ring", func(t *testing.T) {
				first[0] ^= 0xff
				again, _ := k.Key(ctx, "s1")
				if string(again) == string(first) {
					t.Fatal("a returned key aliases the stored one")
				}
			})
		})

		t.Run("when a key is destroyed", func(t *testing.T) {
			if err := k.DestroyKey(ctx, "s1"); err != nil {
				t.Fatalf("DestroyKey failed: %v", err)
			}
			t.Run("then it is unrecoverable", func(t *testing.T) {
				if _, err := k.Key(ctx, "s1"); !errors.Is(err, ErrKeyDestroyed) {
					t.Fatalf("Key after destroy = %v; want ErrKeyDestroyed", err)
				}
			})
			t.Run("then destroying again is idempotent", func(t *testing.T) {
				if err := k.DestroyKey(ctx, "s1"); err != nil {
					t.Fatalf("second DestroyKey = %v; want nil", err)
				}
			})
			t.Run("then destroying a subject that never had a key still bars it", func(t *testing.T) {
				if err := k.DestroyKey(ctx, "never-seen"); err != nil {
					t.Fatalf("DestroyKey = %v; want nil", err)
				}
				if _, err := k.Key(ctx, "never-seen"); !errors.Is(err, ErrKeyDestroyed) {
					t.Fatalf("Key = %v; want ErrKeyDestroyed — destruction is terminal", err)
				}
			})
		})

		t.Run("when a subject is empty", func(t *testing.T) {
			t.Run("then both operations refuse", func(t *testing.T) {
				if _, err := k.Key(ctx, ""); err == nil {
					t.Fatal("Key of an empty subject succeeded")
				}
				if err := k.DestroyKey(ctx, ""); err == nil {
					t.Fatal("DestroyKey of an empty subject succeeded")
				}
			})
		})

		t.Run("when the context is cancelled", func(t *testing.T) {
			cctx, cancel := context.WithCancel(context.Background())
			cancel()
			t.Run("then both operations report it", func(t *testing.T) {
				if _, err := k.Key(cctx, "s"); !errors.Is(err, context.Canceled) {
					t.Fatalf("Key = %v; want context.Canceled", err)
				}
				if err := k.DestroyKey(cctx, "s"); !errors.Is(err, context.Canceled) {
					t.Fatalf("DestroyKey = %v; want context.Canceled", err)
				}
			})
		})
	})
}

// storeOf and mustKeyringOf reach into a Log for fixtures that need to build a
// second log over the same backing state.
func storeOf(l *Log) Store         { return l.store }
func mustKeyringOf(l *Log) Keyring { return l.keyring }
