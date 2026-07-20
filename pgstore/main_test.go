package pgstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	// pgx is a test-only dependency. The adapter itself imports no driver, so
	// callers bring whichever one their project already has; this is simply
	// the one the tests are run against.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/zkrebbekx/chronicle"

	"github.com/zkrebbekx/chronicle/pgstore"
)

// Valid-time constants shared by the integration tests. Whole seconds, so that
// timestamptz's microsecond resolution is never the thing under test.
var (
	march = time.Date(2020, 3, 1, 0, 0, 0, 0, time.UTC)
	april = time.Date(2020, 4, 1, 0, 0, 0, 0, time.UTC)
	may   = time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC)
	june  = time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC)
	july  = time.Date(2020, 7, 1, 0, 0, 0, 0, time.UTC)
)

var (
	alice = chronicle.Actor{ID: "u-alice", Type: "user", Name: "Alice"}
	bob   = chronicle.Actor{ID: "u-bob", Type: "service", Name: "Bob"}
)

// DSNEnv names the environment variable holding the test database's
// connection string. Without it every integration test skips, so the suite
// passes on a machine with no database — which is the difference between a
// test suite people run and one they learn to ignore.
const DSNEnv = "CHRONICLE_TEST_DSN"

// schemaSeq numbers the per-test schemas so that tests running in parallel,
// and separate `go test` invocations against one database, cannot collide.
var schemaSeq atomic.Uint64

// fixtureT is the reporting surface the fixture helpers need. It is an
// interface rather than *testing.T because the conformance suite hands its
// factory a [chroniclefest.T], and both satisfy this.
type fixtureT interface {
	Helper()
	Cleanup(func())
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// testDB opens the shared connection pool, or skips the test if no DSN is
// configured.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		t.Skipf("set %s to run the Postgres integration tests", DSNEnv)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("opening %s: %v", DSNEnv, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("connecting to the test database: %v", err)
	}
	return db
}

// newStore returns a migrated store in a schema of its own, dropped when the
// test finishes.
//
// A schema per store rather than a table prefix, because dropping a schema
// takes the table, its indexes and its constraints with it in one statement,
// and because it makes a leaked fixture obvious rather than merely untidy.
func newStore(t fixtureT, db *sql.DB) *pgstore.Store {
	t.Helper()
	store, _ := newStoreNamed(t, db)
	return store
}

// newStoreNamed is [newStore] plus the schema it landed in, for tests that need
// to open a second store over the same table through a different pool.
func newStoreNamed(t fixtureT, db *sql.DB) (*pgstore.Store, string) {
	t.Helper()
	schema := fmt.Sprintf("chronicle_test_%d_%d", os.Getpid(), schemaSeq.Add(1))

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("creating schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`); err != nil {
			t.Errorf("dropping schema %s: %v", schema, err)
		}
	})

	store, err := pgstore.New(db, pgstore.WithSchema(schema))
	if err != nil {
		t.Fatalf("pgstore.New: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return store, schema
}

// attach opens another store over an existing schema's table, through a
// different connection pool. It does not migrate: the table is already there,
// and the point is to have two genuinely independent handles on it.
func attach(t fixtureT, db *sql.DB, schema string) *pgstore.Store {
	t.Helper()
	store, err := pgstore.New(db, pgstore.WithSchema(schema))
	if err != nil {
		t.Fatalf("pgstore.New: %v", err)
	}
	return store
}
