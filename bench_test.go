package chronicle

import (
	"context"
	"fmt"
	"testing"
	"time"
)

var (
	benchRecord *Record
	benchRecs   []Record
	benchCursor Cursor
	benchDelta  Delta
	benchResult Result
)

func benchActor() Actor { return Actor{ID: "bench", Type: "service"} }

// BenchmarkPutNoOverlap is the baseline: each write lands in fresh valid time
// and supersedes nothing.
func BenchmarkPutNoOverlap(b *testing.B) {
	ctx := context.Background()
	l := NewLog(NewMemStore())
	data := []byte(`{"v":1}`)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		from := t0.Add(time.Duration(i) * time.Hour)
		benchResult, _ = l.Put(ctx, employee, "e", data, from, from.Add(time.Hour), benchActor())
	}
}

// BenchmarkPutHeavyOverlap writes repeatedly over one unbounded interval, so
// every write supersedes the current record and splits it. This is the
// expensive shape: a scan, a supersession and up to three inserts per call.
func BenchmarkPutHeavyOverlap(b *testing.B) {
	ctx := context.Background()
	l := NewLog(NewMemStore())
	data := []byte(`{"v":1}`)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Straddle the middle of an ever-growing covered span so that both a
		// left and a right remainder are produced every time.
		from := t0.Add(time.Duration(i%1000) * time.Hour)
		benchResult, _ = l.Put(ctx, employee, "e", data, from, from.Add(500*time.Hour), benchActor())
	}
}

// BenchmarkPutFragmented writes into an entity whose valid timeline has
// already been split into many segments, so each write scans and supersedes
// several records at once.
func BenchmarkPutFragmented(b *testing.B) {
	ctx := context.Background()
	l := NewLog(NewMemStore())
	data := []byte(`{"v":1}`)

	// Lay down 200 adjacent one-hour segments.
	for i := 0; i < 200; i++ {
		from := t0.Add(time.Duration(i) * time.Hour)
		if _, err := l.Put(ctx, employee, "e", data, from, from.Add(time.Hour), benchActor()); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Each write spans 50 existing segments.
		from := t0.Add(time.Duration(i%150) * time.Hour)
		benchResult, _ = l.Put(ctx, employee, "e", data, from, from.Add(50*time.Hour), benchActor())
	}
}

// benchLogWithHistory builds a log with n sequential versions of one entity,
// so that as-of reads have a deep transaction history to search.
func benchLogWithHistory(b *testing.B, n int) *Log {
	b.Helper()
	ctx := context.Background()
	l := NewLog(NewMemStore())
	data := []byte(`{"salary":50000,"title":"engineer","dept":"platform"}`)
	for i := 0; i < n; i++ {
		if _, err := l.Put(ctx, employee, "e", data, t1, time.Time{}, benchActor()); err != nil {
			b.Fatal(err)
		}
	}
	return l
}

func BenchmarkGetAsOfLongHistory(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("history=%d", n), func(b *testing.B) {
			ctx := context.Background()
			l := benchLogWithHistory(b, n)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchRecord, _ = l.Get(ctx, employee, "e", As{ValidAt: t2})
			}
		})
	}
}

// BenchmarkGetAsOfMidHistory reads at a transaction instant in the middle of
// the history rather than at the end, which is the query a uni-temporal system
// cannot answer at all.
func BenchmarkGetAsOfMidHistory(b *testing.B) {
	ctx := context.Background()
	l := NewLog(NewMemStore())
	data := []byte(`{"salary":50000}`)

	var mid time.Time
	for i := 0; i < 2000; i++ {
		res, err := l.Put(ctx, employee, "e", data, t1, time.Time{}, benchActor())
		if err != nil {
			b.Fatal(err)
		}
		if i == 1000 {
			mid = res.TxAt
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchRecord, _ = l.Get(ctx, employee, "e", As{ValidAt: t2, TxAt: mid})
	}
}

func BenchmarkQueryPaginated(b *testing.B) {
	for _, pageSize := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("page=%d", pageSize), func(b *testing.B) {
			ctx := context.Background()
			l := NewLog(NewMemStore())
			data := []byte(`{"v":1}`)
			for i := 0; i < 5000; i++ {
				from := t0.Add(time.Duration(i) * time.Hour)
				if _, err := l.Put(ctx, employee, fmt.Sprintf("e-%d", i%50), data, from, from.Add(time.Hour), benchActor()); err != nil {
					b.Fatal(err)
				}
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				cursor := NoCursor
				for {
					recs, next, err := l.Query(ctx, Query{Kind: employee, Limit: pageSize, After: cursor})
					if err != nil {
						b.Fatal(err)
					}
					benchRecs = recs
					if next.IsZero() {
						break
					}
					cursor = next
				}
			}
		})
	}
}

func BenchmarkQueryFirstPage(b *testing.B) {
	ctx := context.Background()
	l := NewLog(NewMemStore())
	data := []byte(`{"v":1}`)
	for i := 0; i < 5000; i++ {
		from := t0.Add(time.Duration(i) * time.Hour)
		if _, err := l.Put(ctx, employee, fmt.Sprintf("e-%d", i%50), data, from, from.Add(time.Hour), benchActor()); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchRecs, benchCursor, _ = l.Query(ctx, Query{Kind: employee, Limit: 100})
	}
}

func BenchmarkTimeline(b *testing.B) {
	ctx := context.Background()
	l := NewLog(NewMemStore())
	data := []byte(`{"v":1}`)
	for i := 0; i < 500; i++ {
		from := t0.Add(time.Duration(i) * time.Hour)
		if _, err := l.Put(ctx, employee, "e", data, from, from.Add(time.Hour), benchActor()); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchRecs, _ = l.Timeline(ctx, employee, "e", Now())
	}
}

func BenchmarkDiff(b *testing.B) {
	ctx := context.Background()
	clock := NewFixedClock(t0)
	l := NewLog(NewMemStore(), WithClock(clock))

	first, err := l.Put(ctx, employee, "e",
		[]byte(`{"salary":50000,"title":"engineer","address":{"city":"Leeds","postcode":"LS1"},"tags":["a","b","c"]}`),
		t1, time.Time{}, benchActor())
	if err != nil {
		b.Fatal(err)
	}
	clock.Advance(24 * time.Hour)
	second, err := l.Correct(ctx, employee, "e",
		[]byte(`{"salary":60000,"title":"engineer","address":{"city":"York","postcode":"LS1"},"tags":["a","z","c"]}`),
		t1, time.Time{}, benchActor())
	if err != nil {
		b.Fatal(err)
	}

	from := As{ValidAt: t2, TxAt: first.TxAt}
	to := As{ValidAt: t2, TxAt: second.TxAt}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchDelta, _ = l.Diff(ctx, employee, "e", from, to)
	}
}

// BenchmarkFieldHistoryLongHistory walks one field back through a long
// transaction history at a fixed valid point. Every correction covers the
// point, so the walk decodes all of them: this is the linear cost the doc
// comment promises, measured.
func BenchmarkFieldHistoryLongHistory(b *testing.B) {
	ctx := context.Background()
	clock := NewFixedClock(t0)
	l := NewLog(NewMemStore(), WithClock(clock))
	for i := 0; i < 500; i++ {
		clock.Set(t0.Add(time.Duration(i) * time.Hour))
		if _, err := l.Correct(ctx, employee, "e",
			[]byte(fmt.Sprintf(`{"salary":%d,"title":"engineer"}`, i)),
			t2, t4, benchActor()); err != nil {
			b.Fatal(err)
		}
	}
	at := As{ValidAt: t2.Add(24 * time.Hour)}

	var sink []FieldRevision
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sink, _ = l.FieldHistory(ctx, employee, "e", "/salary", at)
	}
	_ = sink
}

func BenchmarkIntervalOverlaps(b *testing.B) {
	pairs := []struct{ a, c Interval }{
		{Between(t1, t3), Between(t2, t4)},
		{Since(t1), Between(t2, t3)},
		{Until(t3), Since(t1)},
		{Always(), Between(t1, t2)},
	}
	var sink bool
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := pairs[i%len(pairs)]
		sink = p.a.Overlaps(p.c)
	}
	_ = sink
}

func BenchmarkCursorRoundTrip(b *testing.B) {
	rec := Record{ID: "20260101T000000.000000000Z-000000000042", TxFrom: t1, ValidFrom: t2}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchCursor = EncodeCursor(rec)
		if _, err := DecodeCursor(benchCursor); err != nil {
			b.Fatal(err)
		}
	}
}
