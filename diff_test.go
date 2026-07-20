package chronicle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// changeSet renders a delta as comparable strings, so that assertions read as
// the change list a user would see.
func changeSet(d Delta) []string {
	out := make([]string, len(d.Changes))
	for i, c := range d.Changes {
		out[i] = fmt.Sprintf("%s %s: %s -> %s", c.Op, c.Path, render(c.Old), render(c.New))
	}
	return out
}

func render(v any) string {
	if v == nil {
		return "<nil>"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func TestDiff(t *testing.T) {
	ctx := context.Background()

	t.Run("given two states of an entity", func(t *testing.T) {
		cases := []struct {
			name       string
			from, to   string
			want       []string
			wantNoDiff bool
		}{
			{
				name: "a scalar field changes",
				from: `{"salary":50000,"title":"engineer"}`,
				to:   `{"salary":60000,"title":"engineer"}`,
				want: []string{`modified /salary: 50000 -> 60000`},
			},
			{
				name: "several scalar fields change",
				from: `{"a":1,"b":"x","c":true}`,
				to:   `{"a":2,"b":"y","c":false}`,
				want: []string{
					`modified /a: 1 -> 2`,
					`modified /b: "x" -> "y"`,
					`modified /c: true -> false`,
				},
			},
			{
				name: "a field is added",
				from: `{"a":1}`,
				to:   `{"a":1,"b":2}`,
				want: []string{`added /b: <nil> -> 2`},
			},
			{
				name: "a field is removed",
				from: `{"a":1,"b":2}`,
				to:   `{"a":1}`,
				want: []string{`removed /b: 2 -> <nil>`},
			},
			{
				name: "a field becomes null",
				from: `{"a":1}`,
				to:   `{"a":null}`,
				want: []string{`modified /a: 1 -> <nil>`},
			},
			{
				name:       "nothing changes",
				from:       `{"a":1,"b":[1,2],"c":{"d":3}}`,
				to:         `{"a":1,"b":[1,2],"c":{"d":3}}`,
				wantNoDiff: true,
			},
			{
				name:       "only key order changes",
				from:       `{"a":1,"b":2}`,
				to:         `{"b":2,"a":1}`,
				wantNoDiff: true,
			},
			{
				name: "a nested field changes",
				from: `{"address":{"city":"Leeds","postcode":"LS1"}}`,
				to:   `{"address":{"city":"York","postcode":"LS1"}}`,
				want: []string{`modified /address/city: "Leeds" -> "York"`},
			},
			{
				name: "a deeply nested field changes",
				from: `{"a":{"b":{"c":{"d":1}}}}`,
				to:   `{"a":{"b":{"c":{"d":2}}}}`,
				want: []string{`modified /a/b/c/d: 1 -> 2`},
			},
			{
				name: "a nested field is added",
				from: `{"address":{"city":"Leeds"}}`,
				to:   `{"address":{"city":"Leeds","country":"UK"}}`,
				want: []string{`added /address/country: <nil> -> "UK"`},
			},
			{
				name: "a whole subtree is added",
				from: `{"a":1}`,
				to:   `{"a":1,"address":{"city":"Leeds"}}`,
				want: []string{`added /address: <nil> -> {"city":"Leeds"}`},
			},
			{
				name: "a whole subtree is removed",
				from: `{"a":1,"address":{"city":"Leeds"}}`,
				to:   `{"a":1}`,
				want: []string{`removed /address: {"city":"Leeds"} -> <nil>`},
			},
			{
				name: "a field changes shape from scalar to object",
				from: `{"a":1}`,
				to:   `{"a":{"b":2}}`,
				want: []string{`modified /a: 1 -> {"b":2}`},
			},
			{
				name: "a field changes shape from object to scalar",
				from: `{"a":{"b":2}}`,
				to:   `{"a":1}`,
				want: []string{`modified /a: {"b":2} -> 1`},
			},
			{
				name: "an array element changes",
				from: `{"tags":["a","b","c"]}`,
				to:   `{"tags":["a","z","c"]}`,
				want: []string{`modified /tags/1: "b" -> "z"`},
			},
			{
				name: "an array grows",
				from: `{"tags":["a"]}`,
				to:   `{"tags":["a","b"]}`,
				want: []string{`added /tags/1: <nil> -> "b"`},
			},
			{
				name: "an array shrinks",
				from: `{"tags":["a","b"]}`,
				to:   `{"tags":["a"]}`,
				want: []string{`removed /tags/1: "b" -> <nil>`},
			},
			{
				name: "an array of objects changes at one element",
				from: `{"roles":[{"name":"admin"},{"name":"user"}]}`,
				to:   `{"roles":[{"name":"admin"},{"name":"owner"}]}`,
				want: []string{`modified /roles/1/name: "user" -> "owner"`},
			},
			{
				name: "a field name contains pointer metacharacters",
				from: `{"a/b":1,"c~d":2}`,
				to:   `{"a/b":9,"c~d":8}`,
				want: []string{
					`modified /a~1b: 1 -> 9`,
					`modified /c~0d: 2 -> 8`,
				},
			},
			{
				name: "large integers differ beyond float64 precision",
				from: `{"n":9007199254740993}`,
				to:   `{"n":9007199254740992}`,
				want: []string{`modified /n: 9007199254740993 -> 9007199254740992`},
			},
		}

		for _, tc := range cases {
			t.Run("when "+tc.name, func(t *testing.T) {
				l, _, clock := newTestLog(t)
				clock.Set(t1)
				first := mustPut(t, l, "e", tc.from, t1, time.Time{})
				clock.Set(t3)
				second := mustCorrect(t, l, "e", tc.to, t1, time.Time{})

				d, err := l.Diff(ctx, employee, "e",
					As{ValidAt: t1, TxAt: first.TxAt},
					As{ValidAt: t1, TxAt: second.TxAt})
				if err != nil {
					t.Fatalf("Diff failed: %v", err)
				}

				if tc.wantNoDiff {
					t.Run("then no changes are reported", func(t *testing.T) {
						if !d.IsEmpty() {
							t.Fatalf("Diff reported %v; want no changes", changeSet(d))
						}
					})
					return
				}

				t.Run("then exactly the expected field changes are reported", func(t *testing.T) {
					got := changeSet(d)
					if !equalStrings(got, tc.want) {
						t.Fatalf("changes:\n got %v\nwant %v", got, tc.want)
					}
				})
				t.Run("then the paths are reported in order", func(t *testing.T) {
					paths := d.Paths()
					if len(paths) != len(d.Changes) {
						t.Fatalf("Paths() has %d entries; want %d", len(paths), len(d.Changes))
					}
					for i, p := range paths {
						if p != d.Changes[i].Path {
							t.Fatalf("Paths()[%d] = %q; want %q", i, p, d.Changes[i].Path)
						}
					}
				})
				t.Run("then a specific change is findable by path", func(t *testing.T) {
					c, ok := d.Change(d.Changes[0].Path)
					if !ok || c.Path != d.Changes[0].Path {
						t.Fatalf("Change(%q) did not return the change", d.Changes[0].Path)
					}
					if _, ok := d.Change("/definitely-not-a-field"); ok {
						t.Fatal("Change returned a change for an absent path")
					}
				})
				t.Run("then both records are reported", func(t *testing.T) {
					if d.FromRecord == nil || d.ToRecord == nil {
						t.Fatal("Diff did not report both records")
					}
					if d.Kind != employee || d.EntityID != "e" {
						t.Fatalf("Delta identifies %s/%s; want %s/e", d.Kind, d.EntityID, employee)
					}
				})
			})
		}
	})

	t.Run("given an entity that did not exist at the earlier point", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t1)
		before := clock.Now()
		clock.Set(t2)
		created := mustPut(t, l, "e", `{"a":1,"b":2}`, t1, time.Time{})

		d, err := l.Diff(ctx, employee, "e", As{ValidAt: t1, TxAt: before}, As{ValidAt: t1, TxAt: created.TxAt})
		if err != nil {
			t.Fatalf("Diff failed: %v", err)
		}

		t.Run("when diffed against its creation", func(t *testing.T) {
			t.Run("then every field is reported as added", func(t *testing.T) {
				want := []string{`added /a: <nil> -> 1`, `added /b: <nil> -> 2`}
				if got := changeSet(d); !equalStrings(got, want) {
					t.Fatalf("changes:\n got %v\nwant %v", got, want)
				}
			})
			t.Run("then the earlier record is nil", func(t *testing.T) {
				if d.FromRecord != nil {
					t.Fatal("FromRecord should be nil where the entity did not exist")
				}
				if d.ToRecord == nil {
					t.Fatal("ToRecord should be populated")
				}
			})
		})

		t.Run("when diffed in the other direction", func(t *testing.T) {
			t.Run("then every field is reported as removed", func(t *testing.T) {
				rev, err := l.Diff(ctx, employee, "e", As{ValidAt: t1, TxAt: created.TxAt}, As{ValidAt: t1, TxAt: before})
				if err != nil {
					t.Fatalf("Diff failed: %v", err)
				}
				want := []string{`removed /a: 1 -> <nil>`, `removed /b: 2 -> <nil>`}
				if got := changeSet(rev); !equalStrings(got, want) {
					t.Fatalf("changes:\n got %v\nwant %v", got, want)
				}
			})
		})
	})

	t.Run("given an entity that exists at neither point", func(t *testing.T) {
		l, _, _ := newTestLog(t)
		t.Run("when the two points are diffed", func(t *testing.T) {
			t.Run("then it reports not found rather than an empty diff", func(t *testing.T) {
				_, err := l.Diff(ctx, employee, "nobody", As{ValidAt: t1}, As{ValidAt: t2})
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("Diff of a nonexistent entity = %v; want ErrNotFound", err)
				}
			})
		})
	})

	t.Run("given a record whose data the codec cannot decode", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t1)
		first := mustPut(t, l, "e", `{"a":1}`, t1, time.Time{})
		clock.Set(t2)
		second := mustPut(t, l, "e", `this is not json`, t1, time.Time{})

		t.Run("when the two points are diffed", func(t *testing.T) {
			_, err := l.Diff(ctx, employee, "e", As{ValidAt: t1, TxAt: first.TxAt}, As{ValidAt: t1, TxAt: second.TxAt})

			t.Run("then it reports a codec error rather than an empty diff", func(t *testing.T) {
				if !errors.Is(err, ErrCodec) {
					t.Fatalf("Diff over undecodable data = %v; want ErrCodec — under-reporting a "+
						"change is the one failure mode a change log must not have", err)
				}
			})
			t.Run("then the error names the codec and the record", func(t *testing.T) {
				var ce *CodecError
				if !errors.As(err, &ce) {
					t.Fatalf("error = %v; want a *CodecError", err)
				}
				if ce.Codec != "json" || ce.RecordID == "" {
					t.Fatalf("CodecError = %+v; want the codec name and a record ID", ce)
				}
				if ce.Error() == "" {
					t.Fatal("CodecError.Error() is empty")
				}
			})
		})

		t.Run("when the undecodable record is on the earlier side", func(t *testing.T) {
			t.Run("then it still reports a codec error", func(t *testing.T) {
				_, err := l.Diff(ctx, employee, "e", As{ValidAt: t1, TxAt: second.TxAt}, As{ValidAt: t1, TxAt: first.TxAt})
				if !errors.Is(err, ErrCodec) {
					t.Fatalf("Diff = %v; want ErrCodec", err)
				}
			})
		})
	})

	t.Run("given a diff across the valid axis at one belief instant", func(t *testing.T) {
		l, _, clock := newTestLog(t)
		clock.Set(t0)
		mustPut(t, l, "e", `{"salary":50000}`, t1, t3)
		mustPut(t, l, "e", `{"salary":60000}`, t3, time.Time{})

		t.Run("when two valid instants are compared", func(t *testing.T) {
			t.Run("then it reports what actually happened to the entity", func(t *testing.T) {
				d, err := l.Diff(ctx, employee, "e", As{ValidAt: t2}, As{ValidAt: t4})
				if err != nil {
					t.Fatalf("Diff failed: %v", err)
				}
				want := []string{`modified /salary: 50000 -> 60000`}
				if got := changeSet(d); !equalStrings(got, want) {
					t.Fatalf("changes:\n got %v\nwant %v", got, want)
				}
			})
			t.Run("then the resolved points are reported back", func(t *testing.T) {
				d, err := l.Diff(ctx, employee, "e", As{ValidAt: t2}, As{ValidAt: t4})
				if err != nil {
					t.Fatalf("Diff failed: %v", err)
				}
				if d.From.TxAt.IsZero() || d.To.TxAt.IsZero() {
					t.Fatal("Diff did not resolve the unset transaction instants")
				}
			})
		})
	})

	t.Run("given a log with no codec", func(t *testing.T) {
		l := NewLog(NewMemStore())
		l.codec = nil
		if _, err := l.Put(ctx, employee, "e", []byte(`{"a":1}`), t1, time.Time{}, alice); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
		t.Run("when a diff is attempted", func(t *testing.T) {
			t.Run("then it reports a codec error", func(t *testing.T) {
				_, err := l.Diff(ctx, employee, "e", As{ValidAt: t1, TxAt: t1}, As{ValidAt: t1})
				if !errors.Is(err, ErrCodec) {
					t.Fatalf("Diff with no codec = %v; want ErrCodec", err)
				}
			})
		})
	})
}

func TestJSONCodec(t *testing.T) {
	c := JSONCodec{}

	t.Run("given the JSON codec", func(t *testing.T) {
		t.Run("when it is named", func(t *testing.T) {
			t.Run("then it reports json", func(t *testing.T) {
				if c.Name() != "json" {
					t.Fatalf("Name() = %q; want json", c.Name())
				}
			})
		})

		t.Run("when decoding a valid object", func(t *testing.T) {
			t.Run("then the fields are returned", func(t *testing.T) {
				m, err := c.Decode([]byte(`{"a":1,"b":"x"}`))
				if err != nil {
					t.Fatalf("Decode failed: %v", err)
				}
				if len(m) != 2 {
					t.Fatalf("decoded %d fields; want 2", len(m))
				}
			})
			t.Run("then numbers keep their exact notation", func(t *testing.T) {
				m, err := c.Decode([]byte(`{"n":9007199254740993}`))
				if err != nil {
					t.Fatalf("Decode failed: %v", err)
				}
				n, ok := m["n"].(json.Number)
				if !ok {
					t.Fatalf("number decoded as %T; want json.Number", m["n"])
				}
				if n.String() != "9007199254740993" {
					t.Fatalf("number = %s; want 9007199254740993 — float64 decoding would lose this", n)
				}
			})
		})

		t.Run("when decoding JSON null", func(t *testing.T) {
			t.Run("then it yields an empty object, so a null body is a usable tombstone", func(t *testing.T) {
				m, err := c.Decode([]byte(`null`))
				if err != nil {
					t.Fatalf("Decode(null) = %v; want no error", err)
				}
				if len(m) != 0 {
					t.Fatalf("Decode(null) = %v; want an empty map", m)
				}
			})
		})

		t.Run("when decoding something it cannot interpret", func(t *testing.T) {
			cases := []struct{ name, in string }{
				{"empty data", ``},
				{"whitespace only", "  \n\t "},
				{"malformed JSON", `{"a":`},
				{"a top-level array", `[1,2,3]`},
				{"a top-level scalar", `42`},
				{"a top-level string", `"hello"`},
				{"trailing data", `{"a":1} {"b":2}`},
			}
			for _, tc := range cases {
				t.Run("then "+tc.name+" is an error", func(t *testing.T) {
					if _, err := c.Decode([]byte(tc.in)); err == nil {
						t.Fatalf("Decode(%q) = nil error; want a failure", tc.in)
					}
				})
			}
		})
	})
}

func TestChangeOpString(t *testing.T) {
	t.Run("given the change operations", func(t *testing.T) {
		t.Run("when rendered", func(t *testing.T) {
			t.Run("then each has a name", func(t *testing.T) {
				cases := map[ChangeOp]string{
					ChangeModified: "modified",
					ChangeAdded:    "added",
					ChangeRemoved:  "removed",
					ChangeOp(99):   "changeop(99)",
				}
				for op, want := range cases {
					if got := op.String(); got != want {
						t.Fatalf("ChangeOp(%d).String() = %q; want %q", op, got, want)
					}
				}
			})
		})
	})
}

func TestIntentString(t *testing.T) {
	t.Run("given the intents", func(t *testing.T) {
		t.Run("when rendered", func(t *testing.T) {
			t.Run("then each has a name", func(t *testing.T) {
				cases := map[Intent]string{
					IntentAssert:     "assert",
					IntentCorrection: "correction",
					IntentRemainder:  "remainder",
					Intent(42):       "intent(42)",
				}
				for i, want := range cases {
					if got := i.String(); got != want {
						t.Fatalf("Intent(%d).String() = %q; want %q", i, got, want)
					}
				}
			})
		})
		t.Run("when validated", func(t *testing.T) {
			t.Run("then only the defined values are accepted", func(t *testing.T) {
				for _, i := range []Intent{IntentAssert, IntentCorrection, IntentRemainder} {
					if !i.valid() {
						t.Fatalf("Intent %v reported invalid", i)
					}
				}
				if Intent(42).valid() {
					t.Fatal("Intent(42) reported valid")
				}
			})
		})
	})
}
