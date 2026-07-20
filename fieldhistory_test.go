package chronicle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// field-history helpers
// ---------------------------------------------------------------------------

// num is the value JSONCodec decodes a number to, so expectations read in the
// same notation the codec produces.
func num(s string) json.Number { return json.Number(s) }

func absent() FieldValue       { return FieldValue{} }
func present(v any) FieldValue { return FieldValue{Value: v, Present: true} }

// assertRevision checks one revision field by field, using Diff's own value
// equality so notation differences do not read as mismatches.
func assertRevision(t *testing.T, got FieldRevision, path string, from, to FieldValue, txAt time.Time, intent Intent, actor Actor) {
	t.Helper()
	if got.Path != path {
		t.Fatalf("path = %q; want %q", got.Path, path)
	}
	if !got.From.equal(from) {
		t.Fatalf("from = %+v; want %+v", got.From, from)
	}
	if !got.To.equal(to) {
		t.Fatalf("to = %+v; want %+v", got.To, to)
	}
	if !got.TxAt.Equal(txAt) {
		t.Fatalf("txAt = %s; want %s", got.TxAt, txAt)
	}
	if got.Intent != intent {
		t.Fatalf("intent = %s; want %s", got.Intent, intent)
	}
	if got.Actor.ID != actor.ID {
		t.Fatalf("actor = %s; want %s", got.Actor.ID, actor.ID)
	}
}

func fieldHistory(t *testing.T, l *Log, id, path string, as As, opts ...FieldHistoryOption) []FieldRevision {
	t.Helper()
	revs, err := l.FieldHistory(context.Background(), employee, id, path, as, opts...)
	if err != nil {
		t.Fatalf("FieldHistory(%s, %q) failed: %v", id, path, err)
	}
	return revs
}

// midMarch is a valid instant inside the [t2, t4) interval the tests write over,
// standing in for the design's "valid March 15".
var midMarch = t2.Add(15 * 24 * time.Hour)

// ---------------------------------------------------------------------------
// the flagship: retroactive correction
// ---------------------------------------------------------------------------

func TestFieldHistoryRetroactiveCorrection(t *testing.T) {
	t.Run("given salary asserted then retroactively corrected", func(t *testing.T) {
		l, _, clock := newTestLog(t)

		clock.Set(t0) // recorded first
		first := mustPut(t, l, "e", `{"salary":100}`, t2, t4)
		clock.Set(t1) // corrected later
		corrected := mustCorrect(t, l, "e", `{"salary":120}`, t2, t4)

		t.Run("when the salary field is walked at the fixed valid point", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/salary", As{ValidAt: midMarch})

			t.Run("then it returns exactly two revisions", func(t *testing.T) {
				if len(revs) != 2 {
					t.Fatalf("revisions = %d; want 2: %+v", len(revs), revs)
				}
			})
			t.Run("then the first is the original assertion, absent to 100", func(t *testing.T) {
				assertRevision(t, revs[0], "/salary", absent(), present(num("100")), first.TxAt, IntentAssert, alice)
			})
			t.Run("then the second is the correction, 100 to 120, at the later instant", func(t *testing.T) {
				assertRevision(t, revs[1], "/salary", present(num("100")), present(num("120")), corrected.TxAt, IntentCorrection, bob)
				if !revs[1].TxAt.After(revs[0].TxAt) {
					t.Fatalf("the correction's txAt %s is not after the assertion's %s", revs[1].TxAt, revs[0].TxAt)
				}
			})
			t.Run("then the introducing record's valid interval is carried on each revision", func(t *testing.T) {
				for _, r := range revs {
					if !r.ValidFrom.Equal(t2) || !r.ValidTo.Equal(t4) {
						t.Fatalf("valid interval = %s; want %s", Between(r.ValidFrom, r.ValidTo), Between(t2, t4))
					}
				}
			})
		})

		t.Run("when the descending option is given", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/salary", As{ValidAt: midMarch}, FieldHistoryDescending())
			t.Run("then the correction comes first but still reads from 100 to 120", func(t *testing.T) {
				if len(revs) != 2 {
					t.Fatalf("revisions = %d; want 2", len(revs))
				}
				assertRevision(t, revs[0], "/salary", present(num("100")), present(num("120")), corrected.TxAt, IntentCorrection, bob)
				assertRevision(t, revs[1], "/salary", absent(), present(num("100")), first.TxAt, IntentAssert, alice)
			})
		})
	})
}

// ---------------------------------------------------------------------------
// a different field must not appear
// ---------------------------------------------------------------------------

func TestFieldHistoryIsolatesTheField(t *testing.T) {
	t.Run("given only the title changes between two beliefs", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t0)
		first := mustPut(t, l, "e", `{"salary":100,"title":"engineer"}`, t2, t4)
		clock.Set(t1)
		corrected := mustCorrect(t, l, "e", `{"salary":100,"title":"staff engineer"}`, t2, t4)

		t.Run("when the salary field is walked", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/salary", As{ValidAt: midMarch})
			t.Run("then only the original assertion shows, the title change does not", func(t *testing.T) {
				if len(revs) != 1 {
					t.Fatalf("salary revisions = %d; want 1 (the title change is not a salary change): %+v", len(revs), revs)
				}
				assertRevision(t, revs[0], "/salary", absent(), present(num("100")), first.TxAt, IntentAssert, alice)
			})
		})

		t.Run("when the title field is walked", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/title", As{ValidAt: midMarch})
			t.Run("then both the assertion and the correction show", func(t *testing.T) {
				if len(revs) != 2 {
					t.Fatalf("title revisions = %d; want 2", len(revs))
				}
				assertRevision(t, revs[0], "/title", absent(), present("engineer"), first.TxAt, IntentAssert, alice)
				assertRevision(t, revs[1], "/title", present("engineer"), present("staff engineer"), corrected.TxAt, IntentCorrection, bob)
			})
		})
	})
}

// ---------------------------------------------------------------------------
// nested paths
// ---------------------------------------------------------------------------

func TestFieldHistoryNestedPath(t *testing.T) {
	t.Run("given a nested city changing while the zip stays", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t0)
		mustPut(t, l, "e", `{"address":{"city":"NYC","zip":"10001"}}`, t2, t4)
		clock.Set(t1)
		mustCorrect(t, l, "e", `{"address":{"city":"Boston","zip":"10001"}}`, t2, t4)

		t.Run("when /address/city is walked", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/address/city", As{ValidAt: midMarch})
			t.Run("then the city shows two revisions", func(t *testing.T) {
				if len(revs) != 2 {
					t.Fatalf("city revisions = %d; want 2", len(revs))
				}
				if !revs[1].From.equal(present("NYC")) || !revs[1].To.equal(present("Boston")) {
					t.Fatalf("city revision = %+v; want NYC -> Boston", revs[1])
				}
			})
		})
		t.Run("when /address/zip is walked", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/address/zip", As{ValidAt: midMarch})
			t.Run("then the zip shows only its first appearance", func(t *testing.T) {
				if len(revs) != 1 {
					t.Fatalf("zip revisions = %d; want 1 (the zip did not change): %+v", len(revs), revs)
				}
				assertRevision(t, revs[0], "/address/zip", absent(), present("10001"), revs[0].TxAt, IntentAssert, alice)
			})
		})
		t.Run("when the whole address object is walked", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/address", As{ValidAt: midMarch})
			t.Run("then the subtree change is one revision at the node", func(t *testing.T) {
				if len(revs) != 2 {
					t.Fatalf("address revisions = %d; want 2", len(revs))
				}
			})
		})
	})
}

// ---------------------------------------------------------------------------
// absent vs null — the classic subtle bug
// ---------------------------------------------------------------------------

func TestFieldHistoryAbsentVersusNull(t *testing.T) {
	t.Run("given a field that is set, then nulled, then dropped", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t0)
		mustPut(t, l, "e", `{"salary":100}`, t2, t4)
		clock.Set(t1)
		mustCorrect(t, l, "e", `{"salary":null}`, t2, t4)
		clock.Set(t2)
		mustCorrect(t, l, "e", `{"title":"x"}`, t2, t4) // salary dropped entirely

		t.Run("when the salary field is walked", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/salary", As{ValidAt: midMarch})
			t.Run("then there are three revisions: appear, null, absent", func(t *testing.T) {
				if len(revs) != 3 {
					t.Fatalf("revisions = %d; want 3: %+v", len(revs), revs)
				}
			})
			t.Run("then set-to-null is a change and null is present, not absent", func(t *testing.T) {
				if !revs[1].From.equal(present(num("100"))) {
					t.Fatalf("revision 2 from = %+v; want 100", revs[1].From)
				}
				if !revs[1].To.Present {
					t.Fatal("null field reported as absent; null and absent are different facts")
				}
				if !revs[1].To.IsNull() {
					t.Fatalf("revision 2 to = %+v; want an explicit null", revs[1].To)
				}
			})
			t.Run("then dropping the field is a further change, from null to absent", func(t *testing.T) {
				if !revs[2].From.IsNull() {
					t.Fatalf("revision 3 from = %+v; want null", revs[2].From)
				}
				if revs[2].To.Present {
					t.Fatalf("revision 3 to = %+v; want absent", revs[2].To)
				}
			})
		})
	})
}

func TestFieldHistoryPresentToAbsentViaTombstone(t *testing.T) {
	t.Run("given a field present then a null-body tombstone", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t0)
		first := mustPut(t, l, "e", `{"salary":100}`, t2, t4)
		clock.Set(t1)
		tomb := mustCorrect(t, l, "e", `null`, t2, t4) // JSON null body: a usable tombstone

		t.Run("when the salary field is walked", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/salary", As{ValidAt: midMarch})
			t.Run("then it appears then goes absent", func(t *testing.T) {
				if len(revs) != 2 {
					t.Fatalf("revisions = %d; want 2", len(revs))
				}
				assertRevision(t, revs[0], "/salary", absent(), present(num("100")), first.TxAt, IntentAssert, alice)
				assertRevision(t, revs[1], "/salary", present(num("100")), absent(), tomb.TxAt, IntentCorrection, bob)
			})
		})
	})
}

// ---------------------------------------------------------------------------
// Correction to the spec: an ordinary write cannot make coverage lapse.
// ---------------------------------------------------------------------------

func TestFieldHistoryCoverageDoesNotLapseOnBoundedWrite(t *testing.T) {
	// The phase-5 brief suggested a present->absent transition arises when "a
	// later belief bounds validTo before ValidAt". It does not: the superseded
	// record's tail is preserved as a remainder carrying the OLD value, so the
	// point stays covered — by the old belief, not absence. This test pins that
	// behaviour, which is the recorded Correction.
	t.Run("given a field, then a later belief bounded before the valid point", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t0)
		first := mustPut(t, l, "e", `{"salary":100}`, t2, time.Time{}) // [March, unbounded)
		clock.Set(t1)
		// Re-assert a different salary only up to April; midMarch is after t2 but
		// this write ends at t3 (April) — wait, midMarch < t3, so it is covered by
		// the new write, not the remainder. Use a write that ends BEFORE midMarch.
		mustCorrect(t, l, "e", `{"salary":200}`, t2, t2.Add(time.Hour)) // [March 1, March 1+1h)

		t.Run("when the salary field is walked at a point past the new write's end", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/salary", As{ValidAt: midMarch})
			t.Run("then the field never goes absent — the remainder preserves the old value", func(t *testing.T) {
				if len(revs) != 1 {
					t.Fatalf("revisions = %d; want 1 (coverage never lapses at the point): %+v", len(revs), revs)
				}
				assertRevision(t, revs[0], "/salary", absent(), present(num("100")), first.TxAt, IntentAssert, alice)
			})
			t.Run("then no revision reports absence", func(t *testing.T) {
				for _, r := range revs {
					if !r.To.Present {
						t.Fatalf("revision %+v reports absence, but coverage cannot lapse via an ordinary write", r)
					}
				}
			})
		})
	})
}

// ---------------------------------------------------------------------------
// notation-insensitive numeric equality
// ---------------------------------------------------------------------------

func TestFieldHistoryNumberNotation(t *testing.T) {
	t.Run("given a salary re-recorded in different notation", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t0)
		first := mustPut(t, l, "e", `{"salary":100}`, t2, t4)
		clock.Set(t1)
		mustCorrect(t, l, "e", `{"salary":100.0}`, t2, t4) // same number, other notation
		clock.Set(t2)
		mustCorrect(t, l, "e", `{"salary":1e2}`, t2, t4) // still 100

		t.Run("when the salary field is walked", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/salary", As{ValidAt: midMarch})
			t.Run("then only the first appearance is a revision, notation is not a change", func(t *testing.T) {
				if len(revs) != 1 {
					t.Fatalf("revisions = %d; want 1 (100, 100.0 and 1e2 are one number): %+v", len(revs), revs)
				}
				assertRevision(t, revs[0], "/salary", absent(), present(num("100")), first.TxAt, IntentAssert, alice)
			})
		})
	})
}

// ---------------------------------------------------------------------------
// arrays
// ---------------------------------------------------------------------------

func TestFieldHistoryArrayIndex(t *testing.T) {
	t.Run("given an array element changing by position", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t0)
		mustPut(t, l, "e", `{"tags":["a","b"]}`, t2, t4)
		clock.Set(t1)
		mustCorrect(t, l, "e", `{"tags":["a","c"]}`, t2, t4)

		t.Run("when /tags/1 is walked", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/tags/1", As{ValidAt: midMarch})
			t.Run("then it shows appearance then b to c", func(t *testing.T) {
				if len(revs) != 2 {
					t.Fatalf("revisions = %d; want 2", len(revs))
				}
				if !revs[1].From.equal(present("b")) || !revs[1].To.equal(present("c")) {
					t.Fatalf("revision = %+v; want b -> c", revs[1])
				}
			})
		})
		t.Run("when /tags/0 is walked (unchanged element)", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/tags/0", As{ValidAt: midMarch})
			t.Run("then only its first appearance shows", func(t *testing.T) {
				if len(revs) != 1 {
					t.Fatalf("revisions = %d; want 1", len(revs))
				}
			})
		})
		t.Run("when an out-of-range index is walked", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/tags/9", As{ValidAt: midMarch})
			t.Run("then the result is empty, not an error", func(t *testing.T) {
				if len(revs) != 0 {
					t.Fatalf("revisions = %d; want 0 for an out-of-range index", len(revs))
				}
			})
		})
	})
}

// ---------------------------------------------------------------------------
// pointer escaping and the whole-document path
// ---------------------------------------------------------------------------

func TestFieldHistoryPointerEscaping(t *testing.T) {
	t.Run("given keys containing tilde and slash", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t0)
		mustPut(t, l, "e", `{"a/b":1,"c~d":2}`, t2, t4)
		clock.Set(t1)
		mustCorrect(t, l, "e", `{"a/b":9,"c~d":2}`, t2, t4)

		t.Run("when the escaped slash key is walked as a~1b", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/a~1b", As{ValidAt: midMarch})
			t.Run("then the slash key resolves and shows two revisions", func(t *testing.T) {
				if len(revs) != 2 {
					t.Fatalf("revisions = %d; want 2 for /a~1b", len(revs))
				}
			})
		})
		t.Run("when the escaped tilde key is walked as c~0d", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/c~0d", As{ValidAt: midMarch})
			t.Run("then it resolves and shows only its first appearance (unchanged)", func(t *testing.T) {
				if len(revs) != 1 {
					t.Fatalf("revisions = %d; want 1 for /c~0d", len(revs))
				}
			})
		})
	})

	t.Run("given a document that changes as a whole", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t0)
		mustPut(t, l, "e", `{"a":1}`, t2, t4)
		clock.Set(t1)
		mustCorrect(t, l, "e", `{"a":2}`, t2, t4)

		t.Run("when the empty path is walked", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "", As{ValidAt: midMarch})
			t.Run("then the whole document counts as the field, two revisions", func(t *testing.T) {
				if len(revs) != 2 {
					t.Fatalf("revisions = %d; want 2 for the whole-document path", len(revs))
				}
				if !revs[0].To.Present {
					t.Fatal("the whole document should be present at first appearance")
				}
			})
		})
	})
}

// ---------------------------------------------------------------------------
// empties, errors, and edge inputs
// ---------------------------------------------------------------------------

func TestFieldHistoryEdges(t *testing.T) {
	ctx := context.Background()

	t.Run("given an entity with history but no such field", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t0)
		mustPut(t, l, "e", `{"salary":100}`, t2, t4)
		t.Run("when a never-present path is walked", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/bonus", As{ValidAt: midMarch})
			t.Run("then the result is empty, not an error", func(t *testing.T) {
				if len(revs) != 0 {
					t.Fatalf("revisions = %d; want 0", len(revs))
				}
			})
		})
	})

	t.Run("given an unknown entity", func(t *testing.T) {
		l, _, _ := newTestLog(t)
		t.Run("when its field history is asked for", func(t *testing.T) {
			revs, err := l.FieldHistory(ctx, employee, "ghost", "/salary", Now())
			t.Run("then it is empty rather than not-found", func(t *testing.T) {
				if err != nil {
					t.Fatalf("FieldHistory of an unknown entity = %v; want empty and nil", err)
				}
				if len(revs) != 0 {
					t.Fatalf("revisions = %d; want 0", len(revs))
				}
			})
		})
	})

	t.Run("given a malformed pointer", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t0)
		mustPut(t, l, "e", `{"salary":100}`, t2, t4)
		for _, bad := range []string{"salary", "no-leading-slash", "/bad~", "/bad~2", "/x/~"} {
			t.Run("when the path is "+bad, func(t *testing.T) {
				t.Run("then it is ErrInvalidPath, not an empty result", func(t *testing.T) {
					_, err := l.FieldHistory(ctx, employee, "e", bad, As{ValidAt: midMarch})
					if !errors.Is(err, ErrInvalidPath) {
						t.Fatalf("FieldHistory(%q) = %v; want ErrInvalidPath", bad, err)
					}
					var pe *PathError
					if !errors.As(err, &pe) || pe.Path != bad {
						t.Fatalf("error = %v; want a *PathError naming %q", err, bad)
					}
				})
			})
		}
		t.Run("when the path is malformed but the entity is unknown", func(t *testing.T) {
			t.Run("then the path is still validated before the store is touched", func(t *testing.T) {
				_, err := l.FieldHistory(ctx, employee, "ghost", "nope", As{ValidAt: midMarch})
				if !errors.Is(err, ErrInvalidPath) {
					t.Fatalf("FieldHistory = %v; want ErrInvalidPath even for an unknown entity", err)
				}
			})
		})
	})

	t.Run("given a record whose data cannot be decoded", func(t *testing.T) {
		l, store, _ := newTestLog(t)
		// Seed a record covering the point whose Data is not JSON, straight
		// through the store so no write-path codec check intervenes.
		_, err := store.Apply(ctx, ApplyRequest{
			TxAt: t0,
			Plan: StaticWrite(Write{Insert: []Record{{
				ID: "bad", Kind: employee, EntityID: "e", Data: []byte("not json at all"),
				ValidFrom: t2, ValidTo: t4, Actor: alice,
			}}}),
		})
		if err != nil {
			t.Fatalf("seed Apply failed: %v", err)
		}
		t.Run("when its field history is asked for", func(t *testing.T) {
			_, err := l.FieldHistory(ctx, employee, "e", "/salary", As{ValidAt: midMarch})
			t.Run("then it is a codec error, never a silent gap", func(t *testing.T) {
				if !errors.Is(err, ErrCodec) {
					t.Fatalf("FieldHistory over undecodable data = %v; want ErrCodec", err)
				}
				var ce *CodecError
				if !errors.As(err, &ce) {
					t.Fatalf("error = %v; want a *CodecError", err)
				}
			})
		})
	})

	t.Run("given an empty entity ID", func(t *testing.T) {
		l, _, _ := newTestLog(t)
		t.Run("then it is ErrMissingEntityID", func(t *testing.T) {
			_, err := l.FieldHistory(ctx, employee, "", "/salary", Now())
			if !errors.Is(err, ErrMissingEntityID) {
				t.Fatalf("FieldHistory = %v; want ErrMissingEntityID", err)
			}
		})
	})

	t.Run("given a log restricted to kinds", func(t *testing.T) {
		l, _, _ := newTestLog(t, WithKinds("invoice"))
		t.Run("then an out-of-list kind is ErrUnknownKind", func(t *testing.T) {
			_, err := l.FieldHistory(ctx, employee, "e", "/salary", Now())
			if !errors.Is(err, ErrUnknownKind) {
				t.Fatalf("FieldHistory = %v; want ErrUnknownKind", err)
			}
		})
	})

	t.Run("given a cancelled context", func(t *testing.T) {
		l, _, _ := newTestLog(t)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		t.Run("then it reports the context error", func(t *testing.T) {
			_, err := l.FieldHistory(cctx, employee, "e", "/salary", Now())
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("FieldHistory = %v; want context.Canceled", err)
			}
		})
	})
}

// ---------------------------------------------------------------------------
// the valid axis is fixed; the transaction axis is walked
// ---------------------------------------------------------------------------

func TestFieldHistoryFixesValidPoint(t *testing.T) {
	t.Run("given two disjoint valid intervals with different salaries", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t0)
		mustPut(t, l, "e", `{"salary":100}`, t2, t3) // [March, April)
		clock.Set(t1)
		mustPut(t, l, "e", `{"salary":900}`, t4, t5) // [May, June), disjoint

		t.Run("when the field is walked at a point in the first interval", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/salary", As{ValidAt: t2.Add(24 * time.Hour)})
			t.Run("then only the belief covering that point shows", func(t *testing.T) {
				if len(revs) != 1 || !revs[0].To.equal(present(num("100"))) {
					t.Fatalf("revisions = %+v; want just 100 at the first interval", revs)
				}
			})
		})
		t.Run("when the field is walked at a point in the second interval", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/salary", As{ValidAt: t4.Add(24 * time.Hour)})
			t.Run("then only the other belief shows", func(t *testing.T) {
				if len(revs) != 1 || !revs[0].To.equal(present(num("900"))) {
					t.Fatalf("revisions = %+v; want just 900 at the second interval", revs)
				}
			})
		})
	})

	t.Run("given as.TxAt is set", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t0)
		mustPut(t, l, "e", `{"salary":100}`, t2, t4)
		clock.Set(t1)
		mustCorrect(t, l, "e", `{"salary":120}`, t2, t4)

		t.Run("when a field history is asked with a bounded as.TxAt", func(t *testing.T) {
			// as.TxAt is ignored — the whole transaction axis is walked — so the
			// result is the same as with TxAt unset.
			withTx := fieldHistory(t, l, "e", "/salary", As{ValidAt: midMarch, TxAt: t0})
			without := fieldHistory(t, l, "e", "/salary", As{ValidAt: midMarch})
			t.Run("then it is ignored: the whole transaction axis is walked either way", func(t *testing.T) {
				if len(withTx) != len(without) || len(withTx) != 2 {
					t.Fatalf("with TxAt = %d revisions, without = %d; want 2 each (TxAt ignored)", len(withTx), len(without))
				}
			})
		})
	})

	t.Run("given a zero As", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t0)
		// A belief covering all of valid time, so "now" falls inside it.
		mustPut(t, l, "e", `{"salary":100}`, time.Time{}, time.Time{})
		t.Run("when the field is walked with the zero As", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/salary", Now())
			t.Run("then the valid point defaults to now and the field is found", func(t *testing.T) {
				if len(revs) != 1 {
					t.Fatalf("revisions = %d; want 1 with the default now valid point", len(revs))
				}
			})
		})
	})
}

// ---------------------------------------------------------------------------
// deep history crosses internal query pages without dropping a step
// ---------------------------------------------------------------------------

func TestFieldHistoryDeepHistoryPaginates(t *testing.T) {
	t.Run("given more corrections than fit in one internal query page", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		const n = fieldHistoryPage + 40 // forces at least two pages
		for i := 0; i < n; i++ {
			clock.Set(t0.Add(time.Duration(i) * time.Hour))
			mustCorrect(t, l, "e", fmt.Sprintf(`{"salary":%d}`, i), t2, t4)
		}

		t.Run("when the field is walked", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/salary", As{ValidAt: midMarch})
			t.Run("then every distinct value is one revision, none dropped at a page boundary", func(t *testing.T) {
				if len(revs) != n {
					t.Fatalf("revisions = %d; want %d — a page boundary dropped or repeated a step", len(revs), n)
				}
				for i, r := range revs {
					if !r.To.equal(present(num(fmt.Sprintf("%d", i)))) {
						t.Fatalf("revision %d to = %+v; want %d — the walk lost transaction order across pages", i, r.To, i)
					}
				}
			})
		})
	})
}

// ---------------------------------------------------------------------------
// a shredded belief in the walk fails loudly
// ---------------------------------------------------------------------------

func TestFieldHistoryShredded(t *testing.T) {
	ctx := context.Background()
	t.Run("given a belief encrypted for a subject whose key is then destroyed", func(t *testing.T) {
		keyring := NewMemKeyring()
		clock := NewFixedClock(t0)
		l := NewLog(NewMemStore(), WithClock(clock), WithKeyring(keyring))

		if _, err := l.Put(ctx, employee, "e", []byte(`{"salary":100}`), t2, t4, alice, WithSubject("subj")); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		if err := keyring.DestroyKey(ctx, "subj"); err != nil {
			t.Fatalf("DestroyKey failed: %v", err)
		}

		t.Run("when the field history is asked for", func(t *testing.T) {
			_, err := l.FieldHistory(ctx, employee, "e", "/salary", As{ValidAt: midMarch})
			t.Run("then it is a shred error, never returned as ciphertext or a gap", func(t *testing.T) {
				if !errors.Is(err, ErrShredded) {
					t.Fatalf("FieldHistory over a shredded belief = %v; want ErrShredded", err)
				}
			})
		})
	})
}

// ---------------------------------------------------------------------------
// a path that descends through a scalar is absent, not an error
// ---------------------------------------------------------------------------

func TestFieldHistoryDescendThroughScalar(t *testing.T) {
	t.Run("given a scalar salary", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t0)
		mustPut(t, l, "e", `{"salary":100}`, t2, t4)
		t.Run("when a path tries to descend into it", func(t *testing.T) {
			revs := fieldHistory(t, l, "e", "/salary/deep", As{ValidAt: midMarch})
			t.Run("then the result is empty, not an error", func(t *testing.T) {
				if len(revs) != 0 {
					t.Fatalf("revisions = %d; want 0 for a path through a scalar", len(revs))
				}
			})
		})
	})
}

// ---------------------------------------------------------------------------
// a store error surfaces, never a silent partial answer
// ---------------------------------------------------------------------------

// errStore wraps a Store and fails Query, to prove FieldHistory surfaces a
// store failure rather than returning the records it managed to read.
type errStore struct {
	Store
	err error
}

func (s errStore) Query(context.Context, Query) ([]Record, Cursor, error) {
	return nil, NoCursor, s.err
}

func TestFieldHistoryStoreError(t *testing.T) {
	t.Run("given a store whose Query fails", func(t *testing.T) {
		boom := errors.New("boom")
		l := NewLog(errStore{Store: NewMemStore(), err: boom})
		t.Run("when a field history is asked for", func(t *testing.T) {
			_, err := l.FieldHistory(context.Background(), employee, "e", "/salary", As{ValidAt: midMarch})
			t.Run("then the store error surfaces", func(t *testing.T) {
				if !errors.Is(err, boom) {
					t.Fatalf("FieldHistory = %v; want the store error", err)
				}
			})
		})
	})
}
