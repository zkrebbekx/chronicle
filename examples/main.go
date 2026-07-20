// Command examples demonstrates chronicle's bitemporal model end to end: a
// payroll history that gets a raise, then a retroactive correction, and the
// audit questions each one makes answerable.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zkrebbekx/chronicle"
)

var (
	jan   = date(2026, 1, 1)
	march = date(2026, 3, 1)
	april = date(2026, 4, 1)
	june  = date(2026, 6, 1)
	sept  = date(2026, 9, 1)
)

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	// A fixed clock makes the transaction axis legible in the output. In
	// production you would leave the default, which is time.Now().UTC().
	clock := chronicle.NewFixedClock(jan)
	store := chronicle.NewMemStore()
	lg := chronicle.NewLog(store,
		chronicle.WithClock(clock),
		chronicle.WithKinds("employee"),
	)

	hr := chronicle.Actor{ID: "u-42", Type: "user", Name: "Dana (HR)"}
	audit := chronicle.Actor{ID: "u-99", Type: "user", Name: "Priya (Audit)"}

	// -----------------------------------------------------------------
	// 1. Hiring: a salary valid from January, with no known end.
	// -----------------------------------------------------------------
	hired, err := lg.Put(ctx, "employee", "alice",
		[]byte(`{"salary":50000,"title":"Engineer","address":{"city":"Leeds"}}`),
		jan, time.Time{}, hr,
		chronicle.WithReason("initial hire"),
		chronicle.WithMetaValue("source", "workday"),
	)
	if err != nil {
		return err
	}
	section("1. Hired in January")
	fmt.Printf("   wrote %s at tx %s\n", hired.Record.Valid(), ts(hired.TxAt))

	// -----------------------------------------------------------------
	// 2. A raise, effective June, entered in April. The valid interval
	//    starts in the future relative to the transaction instant.
	// -----------------------------------------------------------------
	clock.Set(april)
	raise, err := lg.Put(ctx, "employee", "alice",
		[]byte(`{"salary":65000,"title":"Senior Engineer","address":{"city":"Leeds"}}`),
		june, time.Time{}, hr,
		chronicle.WithReason("promotion"),
	)
	if err != nil {
		return err
	}
	section("2. Raise entered in April, effective June")
	fmt.Printf("   superseded %d record(s); wrote %d\n", len(raise.Superseded), len(raise.Written))
	printTimeline(ctx, lg, chronicle.Now())

	// -----------------------------------------------------------------
	// 3. A retroactive correction: the January figure was always wrong.
	// -----------------------------------------------------------------
	clock.Set(sept)
	corrected, err := lg.Correct(ctx, "employee", "alice",
		[]byte(`{"salary":55000,"title":"Engineer","address":{"city":"York"}}`),
		jan, june, audit,
		chronicle.WithReason("payroll audit: starting salary was misrecorded"),
	)
	if err != nil {
		return err
	}
	section("3. Retroactive correction in September")
	printTimeline(ctx, lg, chronicle.Now())

	// -----------------------------------------------------------------
	// 4. The bitemporal question. Same stretch of world-time, two
	//    different beliefs, both correctly retrievable.
	// -----------------------------------------------------------------
	section("4. What did we believe, and when?")
	if err := believed(ctx, lg, march, hired.TxAt, "in January"); err != nil {
		return err
	}
	if err := believed(ctx, lg, march, corrected.TxAt, "in September"); err != nil {
		return err
	}

	// -----------------------------------------------------------------
	// 5. What the correction actually changed, field by field.
	// -----------------------------------------------------------------
	section("5. What the correction changed")
	delta, err := lg.Diff(ctx, "employee", "alice",
		chronicle.As{ValidAt: march, TxAt: hired.TxAt},
		chronicle.As{ValidAt: march, TxAt: corrected.TxAt},
	)
	if err != nil {
		return err
	}
	for _, c := range delta.Changes {
		fmt.Printf("   %-9s %-18s %v -> %v\n", c.Op, c.Path, c.Old, c.New)
	}

	// -----------------------------------------------------------------
	// 6. The audit trail: who wrote what, and why.
	// -----------------------------------------------------------------
	section("6. Full history")
	history, err := lg.History(ctx, "employee", "alice")
	if err != nil {
		return err
	}
	for _, r := range history {
		status := "current"
		if !r.IsCurrent() {
			status = "superseded " + ts(r.TxTo)
		}
		fmt.Printf("   %-11s valid %-46s tx %s (%s)\n       by %-14s %s\n",
			r.Intent, r.Valid(), ts(r.TxFrom), status, r.Actor.Name, quote(r.Reason))
	}

	// -----------------------------------------------------------------
	// 7. Cross-entity query with keyset pagination.
	// -----------------------------------------------------------------
	section("7. Paginated query, corrections only")
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("bob-%d", i)
		if _, err := lg.Correct(ctx, "employee", id, []byte(`{"salary":40000}`), jan, time.Time{}, audit); err != nil {
			return err
		}
	}
	var cursor chronicle.Cursor
	page := 0
	for {
		recs, next, err := lg.Query(ctx, chronicle.Query{
			Kind:      "employee",
			Intent:    chronicle.IntentCorrection,
			HasIntent: true,
			Limit:     3,
			After:     cursor,
		})
		if err != nil {
			return err
		}
		page++
		fmt.Printf("   page %d: %d record(s)\n", page, len(recs))
		if next.IsZero() {
			break
		}
		cursor = next
	}

	// -----------------------------------------------------------------
	// 8. What the API refuses to do.
	// -----------------------------------------------------------------
	section("8. Guardrails")
	_, err = lg.Put(ctx, "employee", "carol", []byte(`{}`), jan, june, chronicle.Actor{})
	fmt.Printf("   write with no actor        -> ErrMissingActor: %v\n", errors.Is(err, chronicle.ErrMissingActor))

	_, err = lg.Put(ctx, "employee", "carol", []byte(`{}`), june, jan, hr)
	fmt.Printf("   inverted valid interval    -> ErrInvalidInterval: %v\n", errors.Is(err, chronicle.ErrInvalidInterval))

	_, err = lg.Put(ctx, "employee", "carol", []byte(`{}`), jan, jan, hr)
	fmt.Printf("   empty valid interval       -> ErrInvalidInterval: %v\n", errors.Is(err, chronicle.ErrInvalidInterval))

	_, err = lg.Put(ctx, "porcupine", "carol", []byte(`{}`), jan, june, hr)
	fmt.Printf("   kind outside the allowlist -> ErrUnknownKind: %v\n", errors.Is(err, chronicle.ErrUnknownKind))

	_, _, err = lg.Query(ctx, chronicle.Query{Kind: "employee", After: chronicle.Cursor("nonsense!!")})
	fmt.Printf("   mangled pagination cursor  -> ErrInvalidCursor: %v\n", errors.Is(err, chronicle.ErrInvalidCursor))

	fmt.Println()
	fmt.Printf("%d records in the log; none were ever destroyed.\n", store.Len())
	return nil
}

func believed(ctx context.Context, lg *chronicle.Log, validAt, txAt time.Time, when string) error {
	rec, err := lg.Get(ctx, "employee", "alice", chronicle.As{ValidAt: validAt, TxAt: txAt})
	if err != nil {
		return err
	}
	fmt.Printf("   %-14s we believed March was: %s\n", when, rec.Data)
	return nil
}

func printTimeline(ctx context.Context, lg *chronicle.Log, as chronicle.As) {
	recs, err := lg.Timeline(ctx, "employee", "alice", as)
	if err != nil {
		log.Fatal(err)
	}
	for _, r := range recs {
		fmt.Printf("   %-46s %s\n", r.Valid(), r.Data)
	}
}

func section(title string) {
	fmt.Printf("\n%s\n", title)
	fmt.Println("   " + strings.Repeat("-", len(title)))
}

// ts renders a transaction instant compactly.
func ts(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02T15:04:05.000000000")
}

// quote renders an optional reason, making its absence visible.
func quote(s string) string {
	if s == "" {
		return "(no reason recorded)"
	}
	return strconv.Quote(s)
}
