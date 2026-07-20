package chronicle

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestParsePointer(t *testing.T) {
	t.Run("given well-formed pointers", func(t *testing.T) {
		cases := []struct {
			path string
			want []string
		}{
			{"", nil},
			{"/salary", []string{"salary"}},
			{"/address/city", []string{"address", "city"}},
			{"/a~1b", []string{"a/b"}}, // ~1 unescapes to /
			{"/c~0d", []string{"c~d"}}, // ~0 unescapes to ~
			{"/x~01", []string{"x~1"}}, // ~0 then a literal 1
			{"/tags/0", []string{"tags", "0"}},
			{"/", []string{""}},        // trailing slash: one empty token
			{"/a/", []string{"a", ""}}, // trailing empty token
		}
		for _, tc := range cases {
			t.Run("when parsing "+tc.path, func(t *testing.T) {
				got, err := parsePointer(tc.path)
				t.Run("then it decodes to the expected tokens", func(t *testing.T) {
					if err != nil {
						t.Fatalf("parsePointer(%q) = %v; want no error", tc.path, err)
					}
					if len(got) != len(tc.want) {
						t.Fatalf("tokens = %v; want %v", got, tc.want)
					}
					for i := range tc.want {
						if got[i] != tc.want[i] {
							t.Fatalf("tokens = %v; want %v", got, tc.want)
						}
					}
				})
			})
		}
	})

	t.Run("given malformed pointers", func(t *testing.T) {
		for _, bad := range []string{"salary", "no-slash", "/~", "/~2", "/a~", "/a/~9"} {
			t.Run("when parsing "+bad, func(t *testing.T) {
				t.Run("then it is ErrInvalidPath", func(t *testing.T) {
					_, err := parsePointer(bad)
					if !errors.Is(err, ErrInvalidPath) {
						t.Fatalf("parsePointer(%q) = %v; want ErrInvalidPath", bad, err)
					}
				})
			})
		}
	})
}

func TestValueAtPointer(t *testing.T) {
	doc := map[string]any{
		"salary": json.Number("100"),
		"nested": map[string]any{"city": "NYC"},
		"tags":   []any{"a", "b"},
		"null":   nil,
		"empty":  []any{},
	}

	t.Run("given a decoded document", func(t *testing.T) {
		t.Run("when a present scalar path is resolved", func(t *testing.T) {
			v, present := valueAtPointer(doc, []string{"salary"})
			t.Run("then it is present with the value", func(t *testing.T) {
				if !present || v != json.Number("100") {
					t.Fatalf("got (%v, %v); want (100, true)", v, present)
				}
			})
		})
		t.Run("when the empty path is resolved", func(t *testing.T) {
			v, present := valueAtPointer(doc, nil)
			t.Run("then the whole document is present", func(t *testing.T) {
				if !present {
					t.Fatal("empty path should be present")
				}
				if _, ok := v.(map[string]any); !ok {
					t.Fatalf("empty path value = %T; want the document map", v)
				}
			})
		})
		t.Run("when an explicit null is resolved", func(t *testing.T) {
			v, present := valueAtPointer(doc, []string{"null"})
			t.Run("then it is present with a nil value", func(t *testing.T) {
				if !present || v != nil {
					t.Fatalf("got (%v, %v); want (nil, true)", v, present)
				}
			})
		})
		t.Run("when a path descends into a scalar", func(t *testing.T) {
			_, present := valueAtPointer(doc, []string{"salary", "deep"})
			t.Run("then it is absent, not an error", func(t *testing.T) {
				if present {
					t.Fatal("descending into a scalar should be absent")
				}
			})
		})
		t.Run("when a missing object key is resolved", func(t *testing.T) {
			_, present := valueAtPointer(doc, []string{"missing"})
			t.Run("then it is absent", func(t *testing.T) {
				if present {
					t.Fatal("missing key should be absent")
				}
			})
		})
	})
}

func TestArrayIndex(t *testing.T) {
	t.Run("given an array of length 2", func(t *testing.T) {
		cases := []struct {
			tok     string
			wantIdx int
			wantOK  bool
		}{
			{"0", 0, true},
			{"1", 1, true},
			{"2", 0, false},                     // past the end
			{"9", 0, false},                     // well past the end
			{"", 0, false},                      // empty token
			{"-", 0, false},                     // the end-of-array token names no element
			{"01", 0, false},                    // leading zero is not a valid index
			{"1x", 0, false},                    // trailing non-digit
			{"x", 0, false},                     // non-numeric
			{"999999999999999999999", 0, false}, // would overflow; rejected early
		}
		for _, tc := range cases {
			t.Run("when the token is "+tc.tok, func(t *testing.T) {
				idx, ok := arrayIndex(tc.tok, 2)
				t.Run("then it resolves as expected", func(t *testing.T) {
					if idx != tc.wantIdx || ok != tc.wantOK {
						t.Fatalf("arrayIndex(%q, 2) = (%d, %v); want (%d, %v)", tc.tok, idx, ok, tc.wantIdx, tc.wantOK)
					}
				})
			})
		}
	})

	t.Run("given an empty array", func(t *testing.T) {
		t.Run("when index 0 is resolved", func(t *testing.T) {
			t.Run("then even 0 is out of range", func(t *testing.T) {
				if _, ok := arrayIndex("0", 0); ok {
					t.Fatal("index 0 of an empty array should be out of range")
				}
			})
		})
	})
}
