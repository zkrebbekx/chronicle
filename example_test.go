package chronicle_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zkrebbekx/chronicle"
)

// must unwraps a result in example code, where an error would mean the example
// itself is broken.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

var (
	march = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	april = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	june  = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
)

// A log records what an entity was, and answers questions about it at any
// point on either time axis.
func Example() {
	ctx := context.Background()
	log := chronicle.NewLog(chronicle.NewMemStore())
	hr := chronicle.Actor{ID: "u-42", Type: "user", Name: "Dana"}

	// Alice's salary is 50000 from 1 March, with no known end.
	_, err := log.Put(ctx, "employee", "alice",
		[]byte(`{"salary":50000}`),
		march, time.Time{}, hr)
	if err != nil {
		panic(err)
	}

	rec, err := log.Get(ctx, "employee", "alice", chronicle.Now())
	if err != nil {
		panic(err)
	}
	fmt.Printf("current: %s by %s\n", rec.Data, rec.Actor.Name)

	// Output:
	// current: {"salary":50000} by Dana
}

// A retroactive correction changes what we believe now without changing what
// we are recorded as having believed then.
func ExampleLog_Correct() {
	ctx := context.Background()
	clock := chronicle.NewFixedClock(march)
	log := chronicle.NewLog(chronicle.NewMemStore(), chronicle.WithClock(clock))
	hr := chronicle.Actor{ID: "u-42", Name: "Dana"}

	// In March we record 50000, effective 1 March.
	first := must(log.Put(ctx, "employee", "alice", []byte(`{"salary":50000}`), march, time.Time{}, hr))

	// In April we discover the figure was wrong: it was always 60000.
	clock.Set(april)
	must(log.Correct(ctx, "employee", "alice", []byte(`{"salary":60000}`), march, time.Time{}, hr))

	now := must(log.Get(ctx, "employee", "alice", chronicle.ValidAt(march)))
	then := must(log.Get(ctx, "employee", "alice", chronicle.As{ValidAt: march, TxAt: first.TxAt}))

	fmt.Printf("we now believe March was:      %s\n", now.Data)
	fmt.Printf("in March we believed it was:   %s\n", then.Data)

	// Output:
	// we now believe March was:      {"salary":60000}
	// in March we believed it was:   {"salary":50000}
}

// Writing over part of an existing interval splits it, preserving the parts
// the new record does not cover.
func ExampleLog_Put_overlap() {
	ctx := context.Background()
	log := chronicle.NewLog(chronicle.NewMemStore())
	hr := chronicle.Actor{ID: "u-42"}

	// A grade held from March, indefinitely.
	must(log.Put(ctx, "employee", "alice", []byte(`{"grade":"L3"}`), march, time.Time{}, hr))
	// A temporary promotion covering only April to June.
	must(log.Put(ctx, "employee", "alice", []byte(`{"grade":"L4"}`), april, june, hr))

	timeline := must(log.Timeline(ctx, "employee", "alice", chronicle.Now()))
	for _, r := range timeline {
		fmt.Printf("%s %s\n", r.Valid(), r.Data)
	}

	// Output:
	// [2026-03-01T00:00:00Z, 2026-04-01T00:00:00Z) {"grade":"L3"}
	// [2026-04-01T00:00:00Z, 2026-06-01T00:00:00Z) {"grade":"L4"}
	// [2026-06-01T00:00:00Z, ∞) {"grade":"L3"}
}

// Diff reports field-level changes between two points, descending into nested
// objects.
func ExampleLog_Diff() {
	ctx := context.Background()
	clock := chronicle.NewFixedClock(march)
	log := chronicle.NewLog(chronicle.NewMemStore(), chronicle.WithClock(clock))
	hr := chronicle.Actor{ID: "u-42"}

	before := must(log.Put(ctx, "employee", "alice",
		[]byte(`{"salary":50000,"address":{"city":"Leeds"}}`), march, time.Time{}, hr))
	clock.Set(april)
	after := must(log.Correct(ctx, "employee", "alice",
		[]byte(`{"salary":60000,"address":{"city":"York"},"tenured":true}`), march, time.Time{}, hr))

	delta, err := log.Diff(ctx, "employee", "alice",
		chronicle.As{ValidAt: march, TxAt: before.TxAt},
		chronicle.As{ValidAt: march, TxAt: after.TxAt})
	if err != nil {
		panic(err)
	}
	for _, c := range delta.Changes {
		fmt.Printf("%-8s %-16s %v -> %v\n", c.Op, c.Path, c.Old, c.New)
	}

	// Output:
	// modified /address/city    Leeds -> York
	// modified /salary          50000 -> 60000
	// added    /tenured         <nil> -> true
}

// Query walks the whole log with keyset pagination.
func ExampleLog_Query() {
	ctx := context.Background()
	log := chronicle.NewLog(chronicle.NewMemStore())
	hr := chronicle.Actor{ID: "u-42"}

	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("e-%d", i)
		must(log.Put(ctx, "employee", id, []byte(`{"v":1}`), march, april, hr))
	}

	var cursor chronicle.Cursor
	pages := 0
	total := 0
	for {
		recs, next, err := log.Query(ctx, chronicle.Query{
			Kind:  "employee",
			Limit: 2,
			After: cursor,
		})
		if err != nil {
			panic(err)
		}
		pages++
		total += len(recs)
		if next.IsZero() {
			break
		}
		cursor = next
	}
	fmt.Printf("%d records over %d pages\n", total, pages)

	// Output:
	// 5 records over 3 pages
}

// An actor is required on every write; there is no ambient default.
func ExampleLog_Put_actorRequired() {
	ctx := context.Background()
	log := chronicle.NewLog(chronicle.NewMemStore())

	_, err := log.Put(ctx, "employee", "alice", []byte(`{}`), march, april, chronicle.Actor{})
	fmt.Println(errors.Is(err, chronicle.ErrMissingActor))

	// Output:
	// true
}

// An unbounded end is the zero time, on either axis.
func ExampleInterval() {
	bounded := chronicle.Between(march, june)
	openEnded := chronicle.Since(march)

	fmt.Println(bounded)
	fmt.Println(openEnded)
	fmt.Println(openEnded.Contains(time.Date(2126, 1, 1, 0, 0, 0, 0, time.UTC)))
	fmt.Println(bounded.Overlaps(chronicle.Since(june))) // adjacent, not overlapping

	// Output:
	// [2026-03-01T00:00:00Z, 2026-06-01T00:00:00Z)
	// [2026-03-01T00:00:00Z, ∞)
	// true
	// false
}
