package chronicle

import (
	"errors"
	"testing"
	"time"
)

// Shared instants. Named so that the ordering is obvious at a glance in the
// overlap tables below.
var (
	t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	t3 = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	t4 = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	t5 = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
)

func TestInterval(t *testing.T) {
	t.Run("given a bounded interval", func(t *testing.T) {
		iv := Between(t1, t3)

		t.Run("when asked about its bounds", func(t *testing.T) {
			t.Run("then neither end is unbounded", func(t *testing.T) {
				if iv.StartsUnbounded() || iv.EndsUnbounded() || iv.IsAlways() {
					t.Fatalf("bounded interval %s reported an unbounded end", iv)
				}
			})
			t.Run("then its duration is finite", func(t *testing.T) {
				d, ok := iv.Duration()
				if !ok || d != t3.Sub(t1) {
					t.Fatalf("Duration() = %v, %v; want %v, true", d, ok, t3.Sub(t1))
				}
			})
		})

		t.Run("when testing containment", func(t *testing.T) {
			t.Run("then the lower bound is included", func(t *testing.T) {
				if !iv.Contains(t1) {
					t.Fatal("half-open interval must include its lower bound")
				}
			})
			t.Run("then the upper bound is excluded", func(t *testing.T) {
				if iv.Contains(t3) {
					t.Fatal("half-open interval must exclude its upper bound")
				}
			})
			t.Run("then interior instants are included", func(t *testing.T) {
				if !iv.Contains(t2) {
					t.Fatal("interior instant not contained")
				}
			})
			t.Run("then instants outside are excluded", func(t *testing.T) {
				if iv.Contains(t0) || iv.Contains(t4) {
					t.Fatal("instant outside the interval reported as contained")
				}
			})
		})
	})

	t.Run("given intervals with unbounded ends", func(t *testing.T) {
		t.Run("when the end is unbounded", func(t *testing.T) {
			iv := Since(t1)
			t.Run("then every later instant is contained", func(t *testing.T) {
				for _, at := range []time.Time{t1, t2, t5, t5.AddDate(100, 0, 0)} {
					if !iv.Contains(at) {
						t.Fatalf("%s should contain %s", iv, at)
					}
				}
			})
			t.Run("then earlier instants are not", func(t *testing.T) {
				if iv.Contains(t0) {
					t.Fatalf("%s should not contain %s", iv, t0)
				}
			})
			t.Run("then its duration is not finite", func(t *testing.T) {
				if _, ok := iv.Duration(); ok {
					t.Fatal("unbounded interval reported a finite duration")
				}
			})
		})

		t.Run("when the start is unbounded", func(t *testing.T) {
			iv := Until(t3)
			t.Run("then every earlier instant is contained", func(t *testing.T) {
				for _, at := range []time.Time{t0, t1, t2, {}} {
					if !iv.Contains(at) {
						t.Fatalf("%s should contain %s", iv, at)
					}
				}
			})
			t.Run("then the upper bound is still excluded", func(t *testing.T) {
				if iv.Contains(t3) {
					t.Fatal("upper bound must remain exclusive when the start is unbounded")
				}
			})
		})

		t.Run("when both ends are unbounded", func(t *testing.T) {
			iv := Always()
			t.Run("then it is the zero interval", func(t *testing.T) {
				if iv != (Interval{}) || !iv.IsAlways() {
					t.Fatal("Always() must be the zero Interval")
				}
			})
			t.Run("then it contains every instant", func(t *testing.T) {
				for _, at := range []time.Time{{}, t0, t3, t5} {
					if !iv.Contains(at) {
						t.Fatalf("Always() should contain %s", at)
					}
				}
			})
		})
	})

	t.Run("given a pair of intervals", func(t *testing.T) {
		// The complete overlap taxonomy. Every entry is asserted in both
		// directions, since Overlaps must be symmetric.
		cases := []struct {
			name string
			a, b Interval
			want bool
		}{
			{"identical bounded", Between(t1, t3), Between(t1, t3), true},
			{"b nested strictly inside a", Between(t0, t5), Between(t2, t3), true},
			{"a contains b sharing the lower bound", Between(t1, t5), Between(t1, t3), true},
			{"a contains b sharing the upper bound", Between(t1, t5), Between(t3, t5), true},
			{"left overlap", Between(t0, t3), Between(t2, t5), true},
			{"right overlap", Between(t2, t5), Between(t0, t3), true},
			{"adjacent, a then b", Between(t0, t2), Between(t2, t4), false},
			{"adjacent, b then a", Between(t2, t4), Between(t0, t2), false},
			{"disjoint with a gap", Between(t0, t1), Between(t3, t4), false},
			{"unbounded end vs bounded, overlapping", Since(t1), Between(t2, t3), true},
			{"unbounded end vs bounded, disjoint", Since(t3), Between(t0, t1), false},
			{"unbounded end vs bounded, adjacent", Since(t2), Between(t0, t2), false},
			{"bounded vs unbounded end, overlapping", Between(t0, t3), Since(t2), true},
			{"unbounded start vs bounded, overlapping", Until(t3), Between(t1, t5), true},
			{"unbounded start vs bounded, disjoint", Until(t1), Between(t2, t3), false},
			{"unbounded start vs bounded, adjacent", Until(t2), Between(t2, t4), false},
			{"unbounded start vs unbounded end, overlapping", Until(t3), Since(t1), true},
			{"unbounded start vs unbounded end, adjacent", Until(t2), Since(t2), false},
			{"unbounded start vs unbounded end, disjoint", Until(t1), Since(t3), false},
			{"unbounded both vs anything", Always(), Between(t1, t2), true},
			{"unbounded both vs unbounded both", Always(), Always(), true},
			{"unbounded both vs unbounded end", Always(), Since(t5), true},
			{"unbounded both vs unbounded start", Always(), Until(t0), true},
			{"two unbounded ends always overlap", Since(t0), Since(t5), true},
			{"two unbounded starts always overlap", Until(t0), Until(t5), true},
		}

		for _, tc := range cases {
			t.Run("when they are "+tc.name, func(t *testing.T) {
				t.Run("then overlap is reported correctly", func(t *testing.T) {
					if got := tc.a.Overlaps(tc.b); got != tc.want {
						t.Fatalf("%s.Overlaps(%s) = %v; want %v", tc.a, tc.b, got, tc.want)
					}
				})
				t.Run("then overlap is symmetric", func(t *testing.T) {
					if got := tc.b.Overlaps(tc.a); got != tc.want {
						t.Fatalf("%s.Overlaps(%s) = %v; want %v (asymmetric)", tc.b, tc.a, got, tc.want)
					}
				})
			})
		}
	})

	t.Run("given intervals to compare by start", func(t *testing.T) {
		cases := []struct {
			name string
			a, b Interval
			want bool
		}{
			{"a starts earlier", Between(t1, t5), Between(t2, t5), true},
			{"a starts later", Between(t2, t5), Between(t1, t5), false},
			{"equal starts", Between(t1, t5), Between(t1, t3), false},
			{"unbounded start precedes bounded", Until(t5), Between(t1, t5), true},
			{"bounded does not precede unbounded start", Between(t1, t5), Until(t5), false},
			{"two unbounded starts tie", Until(t3), Until(t5), false},
		}
		for _, tc := range cases {
			t.Run("when "+tc.name, func(t *testing.T) {
				t.Run("then StartsBefore agrees", func(t *testing.T) {
					if got := tc.a.StartsBefore(tc.b); got != tc.want {
						t.Fatalf("%s.StartsBefore(%s) = %v; want %v", tc.a, tc.b, got, tc.want)
					}
				})
			})
		}
	})

	t.Run("given intervals to compare by end", func(t *testing.T) {
		cases := []struct {
			name string
			a, b Interval
			want bool
		}{
			{"a ends later", Between(t1, t5), Between(t1, t3), true},
			{"a ends earlier", Between(t1, t3), Between(t1, t5), false},
			{"equal ends", Between(t1, t3), Between(t2, t3), false},
			{"unbounded end extends beyond bounded", Since(t1), Between(t1, t5), true},
			{"bounded does not extend beyond unbounded end", Between(t1, t5), Since(t1), false},
			{"two unbounded ends tie", Since(t1), Since(t3), false},
		}
		for _, tc := range cases {
			t.Run("when "+tc.name, func(t *testing.T) {
				t.Run("then ExtendsBeyond agrees", func(t *testing.T) {
					if got := tc.a.ExtendsBeyond(tc.b); got != tc.want {
						t.Fatalf("%s.ExtendsBeyond(%s) = %v; want %v", tc.a, tc.b, got, tc.want)
					}
				})
			})
		}
	})

	t.Run("given an interval to validate", func(t *testing.T) {
		t.Run("when it is well formed", func(t *testing.T) {
			for _, iv := range []Interval{Between(t1, t3), Since(t1), Until(t3), Always()} {
				t.Run("then "+iv.String()+" is accepted", func(t *testing.T) {
					if err := iv.Validate(); err != nil {
						t.Fatalf("Validate() = %v; want nil", err)
					}
				})
			}
		})

		t.Run("when it is empty", func(t *testing.T) {
			iv := Between(t1, t1)
			t.Run("then it is rejected as an invalid interval", func(t *testing.T) {
				err := iv.Validate()
				if !errors.Is(err, ErrInvalidInterval) {
					t.Fatalf("Validate() = %v; want ErrInvalidInterval", err)
				}
			})
		})

		t.Run("when it is inverted", func(t *testing.T) {
			iv := Between(t3, t1)
			t.Run("then it is rejected as an invalid interval", func(t *testing.T) {
				err := iv.Validate()
				if !errors.Is(err, ErrInvalidInterval) {
					t.Fatalf("Validate() = %v; want ErrInvalidInterval", err)
				}
				var ie *IntervalError
				if !errors.As(err, &ie) {
					t.Fatalf("Validate() = %v; want an *IntervalError", err)
				}
				if ie.Interval != iv {
					t.Fatalf("IntervalError.Interval = %s; want %s", ie.Interval, iv)
				}
				if ie.Error() == "" {
					t.Fatal("IntervalError.Error() is empty")
				}
			})
		})

		t.Run("when it is inverted but the field is named", func(t *testing.T) {
			t.Run("then the message names the field", func(t *testing.T) {
				e := &IntervalError{Field: "valid", Interval: Between(t3, t1), Err: ErrInvalidInterval}
				if got := e.Error(); got == "" || !contains(got, "valid") {
					t.Fatalf("Error() = %q; want it to mention the field", got)
				}
			})
		})
	})

	t.Run("given an interval in a non-UTC zone", func(t *testing.T) {
		zone := time.FixedZone("test", 5*3600)
		iv := Between(t1.In(zone), t3.In(zone))

		t.Run("when normalised to UTC", func(t *testing.T) {
			got := iv.UTC()
			t.Run("then the instants are unchanged", func(t *testing.T) {
				if !got.From.Equal(t1) || !got.To.Equal(t3) {
					t.Fatalf("UTC() = %s; want the same instants as %s", got, iv)
				}
			})
			t.Run("then the location is UTC", func(t *testing.T) {
				if got.From.Location() != time.UTC || got.To.Location() != time.UTC {
					t.Fatal("UTC() left a bound in a non-UTC location")
				}
			})
		})

		t.Run("when an unbounded end is normalised", func(t *testing.T) {
			t.Run("then it stays the zero time rather than becoming year 1 UTC", func(t *testing.T) {
				got := Since(t1.In(zone)).UTC()
				if !got.To.IsZero() {
					t.Fatalf("UTC() turned an unbounded end into %s", got.To)
				}
			})
		})
	})

	t.Run("given an interval to render", func(t *testing.T) {
		t.Run("when it is unbounded at both ends", func(t *testing.T) {
			t.Run("then it renders with infinity symbols", func(t *testing.T) {
				if got := Always().String(); got != "[-∞, ∞)" {
					t.Fatalf("String() = %q; want %q", got, "[-∞, ∞)")
				}
			})
		})
		t.Run("when it is bounded", func(t *testing.T) {
			t.Run("then it renders both instants in RFC 3339", func(t *testing.T) {
				want := "[2026-02-01T00:00:00Z, 2026-04-01T00:00:00Z)"
				if got := Between(t1, t3).String(); got != want {
					t.Fatalf("String() = %q; want %q", got, want)
				}
			})
		})
	})
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
