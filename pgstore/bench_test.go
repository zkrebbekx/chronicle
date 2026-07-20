package pgstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/zkrebbekx/chronicle"
	"github.com/zkrebbekx/chronicle/pgstore"
)

// benchStore is the setup shared by the benchmarks. It skips rather than fails
// when no database is configured, so `go test -bench=.` works everywhere.
func benchStore(b *testing.B) *pgstore.Store {
	b.Helper()
	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		b.Skipf("set %s to run the Postgres benchmarks", DSNEnv)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		b.Fatalf("opening %s: %v", DSNEnv, err)
	}
	b.Cleanup(func() { _ = db.Close() })

	schema := fmt.Sprintf("chronicle_bench_%d_%d", os.Getpid(), schemaSeq.Add(1))
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		b.Fatalf("creating schema: %v", err)
	}
	b.Cleanup(func() { _, _ = db.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`) })

	store, err := pgstore.New(db, pgstore.WithSchema(schema))
	if err != nil {
		b.Fatalf("pgstore.New: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		b.Fatalf("Migrate: %v", err)
	}
	return store
}

var benchActor = chronicle.Actor{ID: "u-bench", Type: "service", Name: "bench"}

// BenchmarkApplyHeavyOverlap writes repeatedly into one entity's timeline, so
// that every write supersedes several records and emits remainders for both
// ends of each. This is the expensive shape: the write reads more rows the
// longer it runs, and each one may turn into two more.
func BenchmarkApplyHeavyOverlap(b *testing.B) {
	store := benchStore(b)
	ctx := context.Background()
	l := chronicle.NewLog(store)
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	// Lay down a fragmented timeline first, so the benchmark measures writes
	// against many overlapping records rather than against an empty entity.
	for i := 0; i < 32; i++ {
		from := base.AddDate(0, i, 0)
		if _, err := l.Put(ctx, "employee", "hot", []byte(`{"seed":true}`), from, from.AddDate(0, 1, 0), benchActor); err != nil {
			b.Fatalf("seeding: %v", err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// A wide interval crossing many existing segments.
		from := base.AddDate(0, i%24, 0)
		if _, err := l.Put(ctx, "employee", "hot",
			[]byte(fmt.Sprintf(`{"n":%d}`, i)), from, from.AddDate(0, 8, 0), benchActor); err != nil {
			b.Fatalf("Put: %v", err)
		}
	}
}

// BenchmarkApplyDisjoint is the same operation with no overlap at all: each
// write lands on its own entity. The gap between this and the heavy-overlap
// case is the cost of the split, not the cost of a write.
func BenchmarkApplyDisjoint(b *testing.B) {
	store := benchStore(b)
	ctx := context.Background()
	l := chronicle.NewLog(store)
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := l.Put(ctx, "employee", fmt.Sprintf("e%d", i),
			[]byte(`{"v":1}`), base, base.AddDate(1, 0, 0), benchActor); err != nil {
			b.Fatalf("Put: %v", err)
		}
	}
}

// BenchmarkGetAsOf reads one point on both axes out of a long history. The
// history is deep on the transaction axis, which is what an as-of query has to
// cut through, so this measures whether the indexes keep a lookup flat as the
// log grows rather than linear in it.
func BenchmarkGetAsOf(b *testing.B) {
	store := benchStore(b)
	ctx := context.Background()
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	seedDeepHistory(ctx, b, store, "deep", 2000, base)

	l := chronicle.NewLog(store)
	at := base.AddDate(0, 6, 0)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := l.Get(ctx, "employee", "deep", chronicle.ValidAt(at)); err != nil {
			b.Fatalf("Get: %v", err)
		}
	}
}

// BenchmarkQueryPaged walks a whole result set a page at a time. Keyset
// pagination should make every page cost the same; a plan that scanned and
// discarded would make the last page cost the whole table, and the per-page
// figure here is where that would show up.
func BenchmarkQueryPaged(b *testing.B) {
	store := benchStore(b)
	ctx := context.Background()
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	const total = 1000
	seedDeepHistory(ctx, b, store, "paged", total, base)

	for _, size := range []int{10, 100, 500} {
		b.Run(fmt.Sprintf("page-%d", size), func(b *testing.B) {
			var pages int
			start := time.Now()
			for i := 0; i < b.N; i++ {
				var cursor chronicle.Cursor
				for {
					_, next, err := store.Query(ctx, chronicle.Query{
						Kind: "employee", EntityID: "paged", Limit: size, After: cursor,
					})
					if err != nil {
						b.Fatalf("Query: %v", err)
					}
					pages++
					if next.IsZero() {
						break
					}
					cursor = next
				}
			}
			// Per-page rather than per-walk, so the three sizes are comparable:
			// a keyset scan should cost roughly the same per row whatever the
			// page size, and differ only in round trips.
			if pages > 0 {
				b.ReportMetric(float64(time.Since(start).Nanoseconds())/float64(pages), "ns/page")
			}
		})
	}
}

// BenchmarkQueryLastPage jumps straight to a cursor near the end of the result
// set. This is the figure that separates keyset pagination from OFFSET: it
// should be indistinguishable from fetching the first page.
func BenchmarkQueryLastPage(b *testing.B) {
	store := benchStore(b)
	ctx := context.Background()
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	seedDeepHistory(ctx, b, store, "tail", 5000, base)

	// Walk once to find a cursor near the end, outside the timed loop.
	var cursor chronicle.Cursor
	for i := 0; i < 48; i++ {
		_, next, err := store.Query(ctx, chronicle.Query{
			Kind: "employee", EntityID: "tail", Limit: 100, After: cursor,
		})
		if err != nil {
			b.Fatalf("Query: %v", err)
		}
		if next.IsZero() {
			break
		}
		cursor = next
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := store.Query(ctx, chronicle.Query{
			Kind: "employee", EntityID: "tail", Limit: 100, After: cursor,
		}); err != nil {
			b.Fatalf("Query: %v", err)
		}
	}
}

// seedDeepHistory writes n generations for one entity in a single write, which
// is far faster than n writes and gives the same shape of data to read.
func seedDeepHistory(ctx context.Context, b *testing.B, store *pgstore.Store, entity string, n int, base time.Time) {
	b.Helper()
	recs := make([]chronicle.Record, 0, n)
	for i := 0; i < n; i++ {
		// All but the last are already superseded, so only one is current and
		// the exclusion constraint is satisfied while the transaction axis is
		// deep.
		rec := chronicle.Record{
			ID:        chronicle.RecordID(fmt.Sprintf("seed-%s-%06d", entity, i)),
			Kind:      "employee",
			EntityID:  entity,
			Data:      []byte(`{"v":1}`),
			ValidFrom: base,
			ValidTo:   base.AddDate(1, 0, 0),
			Actor:     benchActor,
		}
		if i < n-1 {
			rec.TxTo = time.Now().UTC().Add(time.Duration(i)*time.Microsecond).AddDate(1, 0, 0)
		}
		recs = append(recs, rec)
	}
	if _, err := store.Apply(ctx, chronicle.ApplyRequest{
		Plan: chronicle.StaticWrite(chronicle.Write{Insert: recs}),
	}); err != nil {
		b.Fatalf("seeding %s: %v", entity, err)
	}
}
