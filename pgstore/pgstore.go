// Package pgstore is chronicle's PostgreSQL storage adapter.
//
// It implements [chronicle.Store] over [database/sql] and imports no driver.
// Bring your own — pgx, lib/pq, whatever your project already has:
//
//	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
//	store, err := pgstore.New(db)
//	if err := store.Migrate(ctx); err != nil { ... }
//	log := chronicle.NewLog(store)
//
// The adapter lives in its own module so that the root stays free of
// dependencies. Nothing here imports anything outside the standard library and
// chronicle itself.
//
// # What Postgres is doing for us
//
// The point of a database adapter is to move the hard parts into the database,
// not to reimplement them in Go:
//
//   - Non-overlap of an entity's current valid intervals is an exclusion
//     constraint over a GiST index, so it is structurally impossible rather
//     than merely checked. It is DEFERRABLE INITIALLY DEFERRED because a
//     correct write passes through an intermediate state that a per-statement
//     check would reject — see [Store.Apply].
//   - Transaction time is assigned by the database, inside the write's own
//     transaction. No process's clock is authoritative once there is more than
//     one process.
//   - Ordering, filtering and keyset pagination are pushed down into the
//     query, so paging a large log never materialises it.
//
// # Isolation
//
// Read [Store.Apply] before deploying this. chronicle's write path is a
// read-modify-write split across two store calls, which no isolation level can
// make atomic, and the adapter's answer is a per-entity lock plus conflict
// detection plus retry above the store. The consequence callers inherit is
// that a write can fail with [chronicle.ErrConflict] under contention, which
// [chronicle.Log] handles by retrying and callers driving the store directly
// must handle themselves.
//
// # Resolution
//
// Postgres timestamptz holds microseconds. A [time.Time] with nanosecond
// precision is truncated on the way in. Transaction timestamps are assigned by
// the database and so are already microsecond-aligned; caller-supplied valid
// times are not, and round-trip equality holds only to the microsecond.
package pgstore

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zkrebbekx/chronicle"
)

//go:embed schema.sql
var schemaSQL string

// DefaultTable is the table name used when none is configured.
const DefaultTable = "chronicle_records"

// Store is a [chronicle.Store] backed by one PostgreSQL table.
//
// It is safe for concurrent use — it holds no mutable state of its own, and
// every operation is one round trip or one transaction. A Store does not own
// the [sql.DB] it was given and never closes it.
type Store struct {
	db *sql.DB

	// schema is the optional schema name, empty to use the search path.
	schema string
	// table is the bare table name, also the prefix for index and constraint
	// names.
	table string
	// qualified is the quoted, optionally schema-qualified table name, safe to
	// interpolate into SQL.
	qualified string
}

// Option configures a [Store].
type Option func(*config)

type config struct {
	schema string
	table  string
}

// WithSchema puts the table in a named schema rather than wherever the
// connection's search path points.
//
// Useful for keeping an audit log out of the application's own namespace, and
// the cleanest way to isolate parallel test runs from one another.
func WithSchema(name string) Option {
	return func(c *config) { c.schema = name }
}

// WithTable overrides the table name. The default is [DefaultTable].
//
// The name is also the prefix for the index and constraint names, so two
// stores in one schema do not collide.
func WithTable(name string) Option {
	return func(c *config) { c.table = name }
}

// New returns a store over db.
//
// It does not touch the database — call [Store.Migrate], or apply
// [SchemaSQL] through your own migration tool, before use. It fails if db is
// nil or if a configured name is not a plain SQL identifier, since names are
// interpolated into DDL rather than bound as parameters and there is no
// parameter form for an identifier.
func New(db *sql.DB, opts ...Option) (*Store, error) {
	if db == nil {
		return nil, errors.New("pgstore: New requires a non-nil *sql.DB")
	}
	cfg := config{table: DefaultTable}
	for _, opt := range opts {
		opt(&cfg)
	}
	if err := validIdentifier("table", cfg.table); err != nil {
		return nil, err
	}
	qualified := quote(cfg.table)
	if cfg.schema != "" {
		if err := validIdentifier("schema", cfg.schema); err != nil {
			return nil, err
		}
		qualified = quote(cfg.schema) + "." + quote(cfg.table)
	}
	return &Store{db: db, schema: cfg.schema, table: cfg.table, qualified: qualified}, nil
}

// Table returns the quoted, schema-qualified table name the store reads and
// writes. Intended for diagnostics and for tests that need to reach past the
// library.
func (s *Store) Table() string { return s.qualified }

// Migrate creates the table, its indexes and its constraints if they are not
// already there. It is idempotent and safe to run on every boot.
//
// It also runs CREATE EXTENSION IF NOT EXISTS btree_gist, which the exclusion
// constraint needs and which requires a role permitted to create extensions.
// Where that is not how the deployment is shaped, create the extension once
// out of band and the statement becomes a no-op.
//
// Migrate does not create the schema named by [WithSchema]; a schema is a
// deployment decision rather than a library one.
func (s *Store) Migrate(ctx context.Context) error {
	sqlText, err := SchemaSQL(s.schema, s.table)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, sqlText); err != nil {
		return fmt.Errorf("pgstore: migrate %s: %w", s.qualified, err)
	}
	return nil
}

// SchemaSQL returns the DDL for a store with the given schema and table names,
// for callers who would rather feed it to their own migration tool than let
// the library run it. An empty schema leaves the table unqualified.
//
// The statements are idempotent, so the output is safe to apply repeatedly and
// safe to check in as a migration.
func SchemaSQL(schema, table string) (string, error) {
	if table == "" {
		table = DefaultTable
	}
	if err := validIdentifier("table", table); err != nil {
		return "", err
	}
	qualified := quote(table)
	if schema != "" {
		if err := validIdentifier("schema", schema); err != nil {
			return "", err
		}
		qualified = quote(schema) + "." + quote(table)
	}
	out := strings.ReplaceAll(schemaSQL, "$TABLE$", qualified)
	return strings.ReplaceAll(out, "$NAME$", table), nil
}

// validIdentifier rejects anything that is not a plain unquoted SQL
// identifier.
//
// Table and schema names are interpolated into DDL and into every query,
// because SQL has no parameter form for an identifier. Restricting them to
// letters, digits and underscores — rather than trying to escape arbitrary
// input — is the only version of this that is obviously correct.
func validIdentifier(what, name string) error {
	if name == "" {
		return fmt.Errorf("pgstore: %s name is empty", what)
	}
	if len(name) > 63 {
		return fmt.Errorf("pgstore: %s name %q is longer than Postgres allows", what, name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return fmt.Errorf("pgstore: %s name %q is not a plain SQL identifier "+
				"(letters, digits and underscores, not starting with a digit)", what, name)
		}
	}
	return nil
}

func quote(name string) string { return `"` + name + `"` }

// columns is the read projection, in the order [scanRecord] expects.
const columns = `id, kind, entity_id, data, valid_from, valid_to, tx_from, tx_to, ` +
	`actor_id, actor_type, actor_name, reason, intent, meta`

// orderKey is the SQL rendering of chronicle's total order: transaction start,
// then valid start with an unbounded start first, then record ID.
//
// COALESCE rather than NULLS FIRST because the keyset predicate compares
// against these expressions, and a comparison involving NULL yields NULL
// rather than false — every resumed page would silently drop the unbounded
// rows. The indexes are built over the same expression so the ordering is read
// off the index rather than sorted.
const orderKey = `tx_from, COALESCE(valid_from, '-infinity'::timestamptz), id`

// nullTime maps chronicle's zero time — which means unbounded on either axis —
// to SQL NULL, and anything else to UTC.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// fromNullTime is the inverse: SQL NULL becomes the zero time.
func fromNullTime(t sql.NullTime) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time.UTC()
}

// Compile-time assertion that the adapter satisfies the storage contract.
var _ chronicle.Store = (*Store)(nil)
