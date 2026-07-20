package pgstore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zkrebbekx/chronicle"
	"github.com/zkrebbekx/chronicle/retain"

	"github.com/zkrebbekx/chronicle/pgstore"
)

// newKeyring returns a migrated keyring in a schema of its own, dropped when
// the test finishes.
func newKeyring(t fixtureT, db *sql.DB) *pgstore.Keyring {
	t.Helper()
	schema := fmt.Sprintf("chronicle_keys_%d_%d", os.Getpid(), schemaSeq.Add(1))
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("creating schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`); err != nil {
			t.Errorf("dropping schema %s: %v", schema, err)
		}
	})
	k, err := pgstore.NewKeyring(db, pgstore.WithSchema(schema))
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	if err := k.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return k
}

// TestComplianceEndToEnd drives the whole phase-3 surface against a real
// database: a chained, encrypted log; a legal hold that beats a sweep; a
// sweep that destroys on schedule and leaves tombstones; a chain that
// verifies across the gap; and tampering done with real SQL — an UPDATE
// behind chronicle's back — that verification must catch.
func TestComplianceEndToEnd(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	t.Run("given a chained log over Postgres with sweepable history", func(t *testing.T) {
		store, schema := newStoreNamed(t, db)
		keyring := newKeyring(t, db)
		log := chronicle.NewLog(store, chronicle.WithChaining(), chronicle.WithKeyring(keyring))

		// Four generations of one employee, the last one encrypted for a
		// subject; every earlier generation ends up superseded.
		var last chronicle.Result
		for i, payload := range []string{`{"v":1}`, `{"v":2}`, `{"v":3}`} {
			res, err := log.Put(ctx, "employee", "alice", []byte(payload), march, time.Time{}, alice,
				chronicle.WithReason(fmt.Sprintf("generation %d", i+1)))
			if err != nil {
				t.Fatalf("Put %d failed: %v", i, err)
			}
			last = res
		}
		res, err := log.Put(ctx, "employee", "alice", []byte(`{"v":4,"name":"Alice"}`), march, time.Time{}, bob,
			chronicle.WithSubject("subject-alice"))
		if err != nil {
			t.Fatalf("encrypted Put failed: %v", err)
		}
		last = res

		t.Run("when the chain is verified", func(t *testing.T) {
			rep, err := log.Verify(ctx, "employee", "alice")
			if err != nil {
				t.Fatalf("Verify failed: %v", err)
			}
			t.Run("then it is intact across microsecond-resolution storage", func(t *testing.T) {
				if !rep.Intact() {
					t.Fatalf("divergence = %+v", rep.Divergence)
				}
				if rep.ChainedRecords != 4 {
					t.Fatalf("chained = %d; want 4", rep.ChainedRecords)
				}
			})
		})

		t.Run("when a hold is placed and a sweep runs", func(t *testing.T) {
			if _, err := store.PlaceHold(ctx, chronicle.Hold{
				ID:            "matter-7",
				Kind:          "employee",
				EffectiveFrom: march, // backdated: the duty attached before anyone placed the hold
				Reason:        "anticipated litigation",
				PlacedBy:      alice,
			}); err != nil {
				t.Fatalf("PlaceHold failed: %v", err)
			}
			policies := []retain.Policy{{Kind: "employee", KeepFor: time.Nanosecond}}
			rep, err := retain.Execute(ctx, store, policies, last.TxAt.Add(time.Hour))
			if err != nil {
				t.Fatalf("Execute failed: %v", err)
			}
			t.Run("then the hold withholds everything eligible", func(t *testing.T) {
				kr := rep.Kinds[0]
				if kr.Deleted != 0 || len(kr.Withheld) != 3 {
					t.Fatalf("report = %+v; want 3 withheld, none deleted", kr)
				}
				for _, w := range kr.Withheld {
					if w.HoldID != "matter-7" {
						t.Fatalf("withholding = %+v; want it attributed to matter-7", w)
					}
				}
			})

			t.Run("when the hold is released and the sweep re-runs with an archive hook", func(t *testing.T) {
				if _, err := store.ReleaseHold(ctx, "matter-7", bob, "matter settled"); err != nil {
					t.Fatalf("ReleaseHold failed: %v", err)
				}
				var archived []chronicle.RecordID
				// now comes from the store's own transaction instants rather
				// than the host's clock: transaction time is database-assigned
				// here, and this host's clock measurably lags the container's,
				// which a nanosecond KeepFor turns into missed records. Real
				// retention periods dwarf clock skew; a test's must not.
				rep, err := retain.Execute(ctx, store, policies, last.TxAt.Add(time.Hour),
					retain.WithArchive(func(_ context.Context, doomed []chronicle.Record) error {
						for _, r := range doomed {
							archived = append(archived, r.ID)
						}
						return nil
					}))
				if err != nil {
					t.Fatalf("Execute failed: %v", err)
				}
				t.Run("then the superseded records are archived and destroyed", func(t *testing.T) {
					if rep.Kinds[0].Deleted != 3 || len(archived) != 3 {
						t.Fatalf("report = %+v, archived = %d; want 3 and 3", rep.Kinds[0], len(archived))
					}
				})
				t.Run("then the hold's full lifecycle is still on record", func(t *testing.T) {
					holds, err := store.Holds(ctx)
					if err != nil || len(holds) != 1 {
						t.Fatalf("Holds = (%v, %v); want the released hold", holds, err)
					}
					h := holds[0]
					if h.ReleasedAt.IsZero() || h.ReleasedBy.ID != bob.ID || !h.EffectiveFrom.Equal(march) {
						t.Fatalf("hold = %+v; want release recorded and the backdated effective instant intact", h)
					}
				})
				t.Run("then the chain verifies across the tombstoned gap", func(t *testing.T) {
					rep, err := log.Verify(ctx, "employee", "alice")
					if err != nil {
						t.Fatalf("Verify failed: %v", err)
					}
					if !rep.Intact() || rep.Tombstones != 3 || rep.ChainedRecords != 1 {
						t.Fatalf("report = %+v; want one survivor over three tombstones", rep)
					}
				})
			})
		})

		t.Run("when the survivor is read and shredded", func(t *testing.T) {
			got, err := log.Get(ctx, "employee", "alice", chronicle.ValidAt(june))
			if err != nil || !strings.Contains(string(got.Data), "Alice") {
				t.Fatalf("Get = (%s, %v); want the decrypted survivor", got, err)
			}
			if err := keyring.DestroyKey(ctx, "subject-alice"); err != nil {
				t.Fatalf("DestroyKey failed: %v", err)
			}
			t.Run("then the value is unrecoverable and the structure is not", func(t *testing.T) {
				if _, err := log.Get(ctx, "employee", "alice", chronicle.ValidAt(june)); !errors.Is(err, chronicle.ErrShredded) {
					t.Fatalf("Get after shred = %v; want ErrShredded", err)
				}
				recs, err := log.History(ctx, "employee", "alice")
				if err != nil || len(recs) != 1 {
					t.Fatalf("History = (%d, %v); want the record structure intact", len(recs), err)
				}
			})
			t.Run("then the chain is untouched by the shred", func(t *testing.T) {
				rep, err := log.Verify(ctx, "employee", "alice")
				if err != nil || !rep.Intact() {
					t.Fatalf("Verify = (%+v, %v); shredding must not break the chain", rep.Divergence, err)
				}
			})
		})

		t.Run("when a survivor is tampered with by SQL behind chronicle's back", func(t *testing.T) {
			res, err := db.ExecContext(ctx,
				`UPDATE "`+schema+`".`+`"chronicle_records" SET reason = 'innocuous edit' WHERE tx_to IS NULL`)
			if err != nil {
				t.Fatalf("tampering UPDATE failed: %v", err)
			}
			if n, _ := res.RowsAffected(); n != 1 {
				t.Fatalf("tampering UPDATE touched %d rows; want 1", n)
			}
			rep, err := log.Verify(ctx, "employee", "alice")
			if err != nil {
				t.Fatalf("Verify failed: %v", err)
			}
			t.Run("then verification fails naming the edited record", func(t *testing.T) {
				if rep.Intact() {
					t.Fatal("a real SQL UPDATE went undetected")
				}
				if !strings.Contains(rep.Divergence.Reason, "does not match") {
					t.Fatalf("reason = %q; want the hash mismatch", rep.Divergence.Reason)
				}
			})
		})
	})
}

// TestHoldsAcrossConnections exercises the holds surface through two
// independent pools, which is what a sweeper process and an application
// process look like.
func TestHoldsAcrossConnections(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	t.Run("given a hold placed through one connection", func(t *testing.T) {
		store, schema := newStoreNamed(t, db)
		if _, err := store.PlaceHold(ctx, chronicle.Hold{ID: "m1", Kind: "employee", PlacedBy: alice}); err != nil {
			t.Fatalf("PlaceHold failed: %v", err)
		}

		t.Run("when a second pool looks", func(t *testing.T) {
			other := testDB(t)
			attached := attach(t, other, schema)
			holds, err := attached.Holds(ctx)
			t.Run("then the hold is visible there", func(t *testing.T) {
				if err != nil || len(holds) != 1 || holds[0].ID != "m1" {
					t.Fatalf("Holds = (%v, %v); want m1 visible across pools", holds, err)
				}
			})
			t.Run("then releasing there is visible here", func(t *testing.T) {
				if _, err := attached.ReleaseHold(ctx, "m1", bob, "done"); err != nil {
					t.Fatalf("ReleaseHold failed: %v", err)
				}
				back, err := store.Holds(ctx)
				if err != nil || len(back) != 1 || back[0].ReleasedAt.IsZero() {
					t.Fatalf("Holds = (%v, %v); want the release visible", back, err)
				}
			})
		})
	})
}

// TestKeyringMigrateIdempotent and the validation cases below cover the
// adapter plumbing that the conformance suites cannot reach.
func TestKeyringMigrateIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	t.Run("given a migrated keyring", func(t *testing.T) {
		k := newKeyring(t, db)
		t.Run("when Migrate runs again", func(t *testing.T) {
			t.Run("then it is a no-op, not an error", func(t *testing.T) {
				if err := k.Migrate(ctx); err != nil {
					t.Fatalf("second Migrate = %v; want nil", err)
				}
			})
		})
		t.Run("when a key is minted through two pools", func(t *testing.T) {
			first, err := k.Key(ctx, "s1")
			if err != nil {
				t.Fatalf("Key failed: %v", err)
			}
			t.Run("then both see one key", func(t *testing.T) {
				again, err := k.Key(ctx, "s1")
				if err != nil || string(again) != string(first) {
					t.Fatalf("Key = %v; want the same key", err)
				}
			})
		})
	})
}

func TestComplianceValidation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	t.Run("given identifier limits", func(t *testing.T) {
		long := strings.Repeat("x", 60)
		t.Run("when a table name leaves no room for the derived names", func(t *testing.T) {
			t.Run("then New refuses rather than letting Postgres truncate", func(t *testing.T) {
				if _, err := pgstore.New(db, pgstore.WithTable(long)); err == nil {
					t.Fatal("New accepted a table name whose _tombstones twin exceeds 63 bytes")
				}
				if _, err := pgstore.SchemaSQL("", long); err == nil {
					t.Fatal("SchemaSQL accepted the same name")
				}
			})
		})
		t.Run("when the keyring gets a bad identifier", func(t *testing.T) {
			t.Run("then it is rejected the same way", func(t *testing.T) {
				if _, err := pgstore.NewKeyring(db, pgstore.WithTable("bad-name")); err == nil {
					t.Fatal("NewKeyring accepted a non-identifier table name")
				}
				if _, err := pgstore.KeysSchemaSQL("bad schema", ""); err == nil {
					t.Fatal("KeysSchemaSQL accepted a non-identifier schema name")
				}
				if _, err := pgstore.NewKeyring(nil); err == nil {
					t.Fatal("NewKeyring accepted a nil DB")
				}
			})
		})
	})

	t.Run("given malformed tombstone lookups", func(t *testing.T) {
		store := newStore(t, db)
		t.Run("when kind or entity is missing", func(t *testing.T) {
			t.Run("then the errors match the reference store's", func(t *testing.T) {
				if _, err := store.Tombstones(ctx, "", "e1"); !errors.Is(err, chronicle.ErrUnknownKind) {
					t.Fatalf("Tombstones = %v; want ErrUnknownKind", err)
				}
				if _, err := store.Tombstones(ctx, "employee", ""); !errors.Is(err, chronicle.ErrMissingEntityID) {
					t.Fatalf("Tombstones = %v; want ErrMissingEntityID", err)
				}
			})
		})
	})

	t.Run("given a cancelled context", func(t *testing.T) {
		store := newStore(t, db)
		k := newKeyring(t, db)
		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		t.Run("when the compliance surface is used", func(t *testing.T) {
			t.Run("then every operation reports it", func(t *testing.T) {
				if _, err := store.Delete(cctx, []chronicle.RecordID{"x"}); !errors.Is(err, context.Canceled) {
					t.Fatalf("Delete = %v", err)
				}
				if _, err := store.Tombstones(cctx, "employee", "e1"); !errors.Is(err, context.Canceled) {
					t.Fatalf("Tombstones = %v", err)
				}
				if _, err := store.PlaceHold(cctx, chronicle.Hold{ID: "h", PlacedBy: alice}); !errors.Is(err, context.Canceled) {
					t.Fatalf("PlaceHold = %v", err)
				}
				if _, err := store.ReleaseHold(cctx, "h", alice, ""); !errors.Is(err, context.Canceled) {
					t.Fatalf("ReleaseHold = %v", err)
				}
				if _, err := store.Holds(cctx); !errors.Is(err, context.Canceled) {
					t.Fatalf("Holds = %v", err)
				}
				if _, err := k.Key(cctx, "s"); !errors.Is(err, context.Canceled) {
					t.Fatalf("Key = %v", err)
				}
				if err := k.DestroyKey(cctx, "s"); !errors.Is(err, context.Canceled) {
					t.Fatalf("DestroyKey = %v", err)
				}
			})
		})
	})

	t.Run("given an empty deletion", func(t *testing.T) {
		store := newStore(t, db)
		t.Run("when Delete is called with no IDs", func(t *testing.T) {
			t.Run("then it is a no-op", func(t *testing.T) {
				if n, err := store.Delete(ctx, nil); err != nil || n != 0 {
					t.Fatalf("Delete = (%d, %v); want (0, nil)", n, err)
				}
			})
		})
	})
}

// TestComplianceSadPaths drives every compliance operation against a schema
// whose tables were never created, asserting that database failures surface
// as errors rather than as empty results — a sweeper told "no holds" by a
// broken store would destroy exactly what a hold protects.
func TestComplianceSadPaths(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	t.Run("given a store and keyring over an unmigrated schema", func(t *testing.T) {
		schema := fmt.Sprintf("chronicle_bare_%d_%d", os.Getpid(), schemaSeq.Add(1))
		if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
			t.Fatalf("creating schema: %v", err)
		}
		t.Cleanup(func() { _, _ = db.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`) })

		store := attach(t, db, schema)
		k, err := pgstore.NewKeyring(db, pgstore.WithSchema(schema))
		if err != nil {
			t.Fatalf("NewKeyring: %v", err)
		}

		t.Run("when each operation runs", func(t *testing.T) {
			t.Run("then every one reports the failure", func(t *testing.T) {
				if _, err := store.Delete(ctx, []chronicle.RecordID{"x"}); err == nil {
					t.Fatal("Delete over a missing table = nil error")
				}
				if _, err := store.Tombstones(ctx, "employee", "e1"); err == nil {
					t.Fatal("Tombstones over a missing table = nil error")
				}
				if _, err := store.PlaceHold(ctx, chronicle.Hold{ID: "h", PlacedBy: alice}); err == nil {
					t.Fatal("PlaceHold over a missing table = nil error")
				}
				if _, err := store.ReleaseHold(ctx, "h", alice, ""); err == nil {
					t.Fatal("ReleaseHold over a missing table = nil error")
				}
				if _, err := store.Holds(ctx); err == nil {
					t.Fatal("Holds over a missing table = nil error")
				}
				if _, err := k.Key(ctx, "s"); err == nil {
					t.Fatal("Key over a missing table = nil error")
				}
				if err := k.DestroyKey(ctx, "s"); err == nil {
					t.Fatal("DestroyKey over a missing table = nil error")
				}
				if _, err := k.Key(ctx, ""); err == nil {
					t.Fatal("Key of an empty subject = nil error")
				}
				if err := k.DestroyKey(ctx, ""); err == nil {
					t.Fatal("DestroyKey of an empty subject = nil error")
				}
			})
		})
	})

	t.Run("given a keys table corrupted from outside", func(t *testing.T) {
		k := newKeyring(t, db)
		if _, err := k.Key(ctx, "victim"); err != nil {
			t.Fatalf("Key failed: %v", err)
		}
		// Truncate the stored key behind the keyring's back.
		if _, err := db.ExecContext(ctx,
			`UPDATE `+k.Table()+` SET key = '\x0102030405'::bytea WHERE subject = $1`,
			"victim"); err != nil {
			t.Fatalf("corrupting UPDATE failed: %v", err)
		}
		t.Run("when the key is read", func(t *testing.T) {
			t.Run("then the wrong-sized key is an error, not a weaker cipher", func(t *testing.T) {
				if _, err := k.Key(ctx, "victim"); err == nil {
					t.Fatal("Key returned a corrupted, wrong-sized key")
				}
			})
		})
	})

	t.Run("given a schema that does not exist at all", func(t *testing.T) {
		t.Run("when migration is attempted", func(t *testing.T) {
			t.Run("then both migrations report it", func(t *testing.T) {
				store, err := pgstore.New(db, pgstore.WithSchema("chronicle_never_created"))
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				if err := store.Migrate(ctx); err == nil {
					t.Fatal("Migrate into a nonexistent schema = nil error")
				}
				k, err := pgstore.NewKeyring(db, pgstore.WithSchema("chronicle_never_created"))
				if err != nil {
					t.Fatalf("NewKeyring: %v", err)
				}
				if err := k.Migrate(ctx); err == nil {
					t.Fatal("keyring Migrate into a nonexistent schema = nil error")
				}
			})
		})
	})
}
