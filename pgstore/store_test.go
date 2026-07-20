package pgstore_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zkrebbekx/chronicle"
	"github.com/zkrebbekx/chronicle/pgstore"
)

// TestNew covers configuration, which is the part of the adapter that runs
// before there is a database to talk to and so has to be right on its own.
func TestNew(t *testing.T) {
	t.Run("given no database handle", func(t *testing.T) {
		t.Run("when a store is constructed", func(t *testing.T) {
			t.Run("then it refuses rather than failing later", func(t *testing.T) {
				if _, err := pgstore.New(nil); err == nil {
					t.Fatal("New(nil) = nil error; want a refusal")
				}
			})
		})
	})

	t.Run("given a name that is not a plain SQL identifier", func(t *testing.T) {
		// Names are interpolated into DDL because SQL has no parameter form for
		// an identifier, so the only safe rule is a strict one. Each of these
		// is a way someone might try to escape it.
		bad := map[string]string{
			"empty":            "",
			"quote":            `records"; DROP TABLE users; --`,
			"space":            "my records",
			"dot":              "public.records",
			"leading digit":    "1records",
			"hyphen":           "chronicle-records",
			"backslash":        `records\`,
			"unicode":          "réçords",
			"newline":          "records\nmore",
			"too long":         strings.Repeat("r", 64),
			"semicolon":        "records;",
			"parenthesis":      "records()",
			"comment sequence": "records--",
		}
		db := &sql.DB{}
		for name, ident := range bad {
			t.Run("when the table is named with a "+name, func(t *testing.T) {
				t.Run("then it is rejected", func(t *testing.T) {
					if _, err := pgstore.New(db, pgstore.WithTable(ident)); err == nil {
						t.Fatalf("New with table %q was accepted", ident)
					}
				})
			})
			if ident == "" {
				// An empty schema is not a malformed one: it means "wherever
				// the search path points", which is the default.
				continue
			}
			t.Run("when the schema is named with a "+name, func(t *testing.T) {
				t.Run("then it is rejected", func(t *testing.T) {
					if _, err := pgstore.New(db, pgstore.WithSchema(ident)); err == nil {
						t.Fatalf("New with schema %q was accepted", ident)
					}
				})
			})
		}
	})

	t.Run("given valid names", func(t *testing.T) {
		t.Run("when a store is constructed", func(t *testing.T) {
			t.Run("then the table name is quoted and schema-qualified", func(t *testing.T) {
				store, err := pgstore.New(&sql.DB{}, pgstore.WithSchema("audit"), pgstore.WithTable("history"))
				if err != nil {
					t.Fatalf("New failed: %v", err)
				}
				if got := store.Table(); got != `"audit"."history"` {
					t.Fatalf("Table() = %s; want \"audit\".\"history\"", got)
				}
			})
			t.Run("then an unset schema leaves the table unqualified", func(t *testing.T) {
				store, err := pgstore.New(&sql.DB{})
				if err != nil {
					t.Fatalf("New failed: %v", err)
				}
				if got := store.Table(); got != `"`+pgstore.DefaultTable+`"` {
					t.Fatalf("Table() = %s; want the default table quoted", got)
				}
			})
		})
	})
}

// TestSchemaSQL covers the DDL renderer, which callers use directly when they
// manage migrations themselves.
func TestSchemaSQL(t *testing.T) {
	t.Run("given a schema and table", func(t *testing.T) {
		sqlText, err := pgstore.SchemaSQL("audit", "history")
		if err != nil {
			t.Fatalf("SchemaSQL failed: %v", err)
		}

		t.Run("when the DDL is rendered", func(t *testing.T) {
			t.Run("then no placeholder survives", func(t *testing.T) {
				for _, token := range []string{"$TABLE$", "$NAME$"} {
					if strings.Contains(sqlText, token) {
						t.Fatalf("rendered DDL still contains %s", token)
					}
				}
			})
			t.Run("then the table is quoted and qualified everywhere", func(t *testing.T) {
				if !strings.Contains(sqlText, `"audit"."history"`) {
					t.Fatal("rendered DDL does not reference the qualified table")
				}
			})
			t.Run("then index and constraint names are prefixed with the table", func(t *testing.T) {
				for _, want := range []string{"history_no_overlap", "history_order", "history_entity_order"} {
					if !strings.Contains(sqlText, want) {
						t.Fatalf("rendered DDL is missing %s, so two stores in one schema would collide", want)
					}
				}
			})
			t.Run("then the exclusion constraint is deferred", func(t *testing.T) {
				if !strings.Contains(sqlText, "DEFERRABLE INITIALLY DEFERRED") {
					t.Fatal("the exclusion constraint is not deferred, which rejects ordinary correct writes")
				}
			})
			t.Run("then the ID column carries the C collation", func(t *testing.T) {
				// Without it, SQL ordering of record IDs differs from Go's
				// byte-wise compare and pagination stops matching MemStore.
				if !strings.Contains(sqlText, `id          text COLLATE "C"`) {
					t.Fatal("the id column is not declared COLLATE \"C\"")
				}
			})
		})
	})

	t.Run("given no table name", func(t *testing.T) {
		t.Run("when the DDL is rendered", func(t *testing.T) {
			t.Run("then the default table is used", func(t *testing.T) {
				sqlText, err := pgstore.SchemaSQL("", "")
				if err != nil {
					t.Fatalf("SchemaSQL failed: %v", err)
				}
				if !strings.Contains(sqlText, `"`+pgstore.DefaultTable+`"`) {
					t.Fatal("rendered DDL does not use the default table name")
				}
			})
		})
	})

	t.Run("given an invalid name", func(t *testing.T) {
		t.Run("when the DDL is rendered", func(t *testing.T) {
			t.Run("then the table name is rejected", func(t *testing.T) {
				if _, err := pgstore.SchemaSQL("", "bad name"); err == nil {
					t.Fatal("SchemaSQL accepted an invalid table name")
				}
			})
			t.Run("then the schema name is rejected", func(t *testing.T) {
				if _, err := pgstore.SchemaSQL("bad schema", "records"); err == nil {
					t.Fatal("SchemaSQL accepted an invalid schema name")
				}
			})
		})
	})
}

// TestMigrate covers the migration entry point against a real database.
func TestMigrate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	t.Run("given a schema that has already been migrated", func(t *testing.T) {
		store := newStore(t, db)
		t.Run("when it is migrated again", func(t *testing.T) {
			t.Run("then it succeeds, so a boot-time migration is safe", func(t *testing.T) {
				if err := store.Migrate(ctx); err != nil {
					t.Fatalf("second Migrate = %v; want nil", err)
				}
			})
			t.Run("then a third time is still fine", func(t *testing.T) {
				if err := store.Migrate(ctx); err != nil {
					t.Fatalf("third Migrate = %v; want nil", err)
				}
			})
		})
	})

	t.Run("given a schema that does not exist", func(t *testing.T) {
		store, err := pgstore.New(db, pgstore.WithSchema("chronicle_absent_schema"))
		if err != nil {
			t.Fatalf("New failed: %v", err)
		}
		t.Run("when it is migrated", func(t *testing.T) {
			t.Run("then the failure names the table", func(t *testing.T) {
				err := store.Migrate(ctx)
				if err == nil {
					t.Fatal("Migrate into a missing schema succeeded")
				}
				if !strings.Contains(err.Error(), "chronicle_absent_schema") {
					t.Fatalf("Migrate error = %v; want it to name the schema", err)
				}
			})
		})
	})

	t.Run("given a custom table name in a shared schema", func(t *testing.T) {
		// Two stores in one schema must not collide on index or constraint
		// names, which is why those are prefixed with the table.
		_, schema := newStoreNamed(t, db)
		second, err := pgstore.New(db, pgstore.WithSchema(schema), pgstore.WithTable("other_history"))
		if err != nil {
			t.Fatalf("New failed: %v", err)
		}
		t.Run("when the second store is migrated", func(t *testing.T) {
			t.Run("then it does not collide with the first", func(t *testing.T) {
				if err := second.Migrate(ctx); err != nil {
					t.Fatalf("Migrate = %v; two stores in one schema must coexist", err)
				}
			})
			t.Run("then the two hold separate logs", func(t *testing.T) {
				if _, err := second.Apply(ctx, chronicle.ApplyRequest{
					Entity: chronicle.EntityRef{Kind: "employee", EntityID: "e1"},
					Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{{
						ID: "only-in-second", Kind: "employee", EntityID: "e1",
						ValidFrom: march, Actor: alice,
					}}}),
				}); err != nil {
					t.Fatalf("Apply failed: %v", err)
				}
				recs, _, err := second.Query(ctx, chronicle.Query{})
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				if len(recs) != 1 || recs[0].ID != "only-in-second" {
					t.Fatalf("second store holds %d records; want just its own", len(recs))
				}
			})
		})
	})
}

// TestApplyEdgeCases covers the corners of the write path that the conformance
// suite has no vocabulary for, because they are specific to a SQL store.
func TestApplyEdgeCases(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	t.Run("given a request with no plan", func(t *testing.T) {
		store := newStore(t, db)
		t.Run("when it is applied", func(t *testing.T) {
			t.Run("then it is refused rather than treated as an empty write", func(t *testing.T) {
				if _, err := store.Apply(ctx, chronicle.ApplyRequest{}); err == nil {
					t.Fatal("Apply with no Plan succeeded")
				}
			})
		})
	})

	t.Run("given a plan that fails", func(t *testing.T) {
		store := newStore(t, db)
		want := errors.New("the caller changed its mind")
		t.Run("when it is applied", func(t *testing.T) {
			_, err := store.Apply(ctx, chronicle.ApplyRequest{
				Entity: chronicle.EntityRef{Kind: "employee", EntityID: "e1"},
				Plan: func([]chronicle.Record, time.Time) (chronicle.Write, error) {
					return chronicle.Write{}, want
				},
			})
			t.Run("then the planner's error comes back unchanged", func(t *testing.T) {
				if !errors.Is(err, want) {
					t.Fatalf("Apply = %v; want the planner's own error", err)
				}
			})
			t.Run("then the transaction rolled back", func(t *testing.T) {
				recs, _, err := store.Query(ctx, chronicle.Query{})
				if err != nil {
					t.Fatalf("Query failed: %v", err)
				}
				if len(recs) != 0 {
					t.Fatalf("%d records survived a failed plan", len(recs))
				}
			})
		})
	})

	t.Run("given a write whose records overlap each other", func(t *testing.T) {
		// A planner is trusted to produce a coherent write, and this is what
		// happens when it does not: the constraint refuses, rather than the
		// log quietly holding two current records over one instant.
		store := newStore(t, db)
		t.Run("when it is applied", func(t *testing.T) {
			_, err := store.Apply(ctx, chronicle.ApplyRequest{
				Entity: chronicle.EntityRef{Kind: "employee", EntityID: "e1"},
				Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{
					{ID: "a", Kind: "employee", EntityID: "e1", ValidFrom: march, ValidTo: july, Actor: alice},
					{ID: "b", Kind: "employee", EntityID: "e1", ValidFrom: april, ValidTo: june, Actor: alice},
				}}),
			})
			t.Run("then it is reported as a conflict", func(t *testing.T) {
				if !errors.Is(err, chronicle.ErrConflict) {
					t.Fatalf("Apply = %v; want ErrConflict from the exclusion constraint", err)
				}
			})
			t.Run("then neither record landed", func(t *testing.T) {
				if n := countRows(ctx, t, db, store.Table()); n != 0 {
					t.Fatalf("%d rows survived a rejected write", n)
				}
			})
		})
	})

	t.Run("given a write large enough to be split into batches", func(t *testing.T) {
		// The insert chunks at a thousand records to stay under Postgres's
		// bind-parameter ceiling. This crosses that boundary.
		store := newStore(t, db)
		const n = 2500
		recs := make([]chronicle.Record, 0, n)
		for i := 0; i < n; i++ {
			recs = append(recs, chronicle.Record{
				ID:        chronicle.RecordID(pad(i)),
				Kind:      "employee",
				EntityID:  "e" + pad(i),
				Data:      []byte(`{"v":1}`),
				ValidFrom: march,
				Actor:     alice,
			})
		}
		t.Run("when it is applied", func(t *testing.T) {
			t.Run("then every record lands", func(t *testing.T) {
				if _, err := store.Apply(ctx, chronicle.ApplyRequest{
					Plan: chronicle.StaticWrite(chronicle.Write{Insert: recs}),
				}); err != nil {
					t.Fatalf("Apply failed: %v", err)
				}
				if got := countRows(ctx, t, db, store.Table()); got != n {
					t.Fatalf("%d rows; want %d", got, n)
				}
			})
		})
	})

	t.Run("given a record whose data is nil", func(t *testing.T) {
		store := newStore(t, db)
		t.Run("when it is round-tripped", func(t *testing.T) {
			tx, err := store.Apply(ctx, chronicle.ApplyRequest{
				Entity: chronicle.EntityRef{Kind: "employee", EntityID: "e1"},
				Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{{
					ID: "nil-data", Kind: "employee", EntityID: "e1", ValidFrom: march, Actor: alice,
				}}}),
			})
			if err != nil {
				t.Fatalf("Apply failed: %v", err)
			}
			t.Run("then it comes back nil rather than as an empty slice", func(t *testing.T) {
				got, err := store.Get(ctx, chronicle.GetQuery{
					Kind: "employee", EntityID: "e1", ValidAt: may, TxAt: tx,
				})
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if got.Data != nil {
					t.Fatalf("Data = %q; want nil", got.Data)
				}
			})
		})
	})

	t.Run("given metadata that is not valid to encode", func(t *testing.T) {
		store := newStore(t, db)
		t.Run("when a record carries a large metadata map", func(t *testing.T) {
			meta := map[string]string{}
			for i := 0; i < 200; i++ {
				meta[pad(i)] = strings.Repeat("v", 64)
			}
			tx, err := store.Apply(ctx, chronicle.ApplyRequest{
				Entity: chronicle.EntityRef{Kind: "employee", EntityID: "e1"},
				Plan: chronicle.StaticWrite(chronicle.Write{Insert: []chronicle.Record{{
					ID: "meta", Kind: "employee", EntityID: "e1", ValidFrom: march, Actor: alice, Meta: meta,
				}}}),
			})
			if err != nil {
				t.Fatalf("Apply failed: %v", err)
			}
			t.Run("then every key survives the round trip", func(t *testing.T) {
				got, err := store.Get(ctx, chronicle.GetQuery{
					Kind: "employee", EntityID: "e1", ValidAt: may, TxAt: tx,
				})
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				if len(got.Meta) != len(meta) {
					t.Fatalf("meta has %d keys; want %d", len(got.Meta), len(meta))
				}
			})
		})
	})
}

// TestErrorPropagation checks that a store pointed at a table that is not there
// reports the failure rather than pretending the log is empty. An adapter that
// swallows a missing table reads as an entity with no history, which is the
// most dangerous possible answer from an audit log.
func TestErrorPropagation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	store, err := pgstore.New(db, pgstore.WithTable("chronicle_no_such_table"))
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	t.Run("given a store pointed at a table that does not exist", func(t *testing.T) {
		t.Run("when it is read", func(t *testing.T) {
			t.Run("then Get reports the failure rather than not-found", func(t *testing.T) {
				_, err := store.Get(ctx, chronicle.GetQuery{Kind: "employee", EntityID: "e1"})
				if err == nil {
					t.Fatal("Get against a missing table succeeded")
				}
				if errors.Is(err, chronicle.ErrNotFound) {
					t.Fatal("a missing table was reported as a missing record, which reads as " +
						"an entity with no history")
				}
			})
			t.Run("then Query reports the failure rather than emptiness", func(t *testing.T) {
				if _, _, err := store.Query(ctx, chronicle.Query{}); err == nil {
					t.Fatal("Query against a missing table succeeded")
				}
			})
			t.Run("then Apply reports the failure", func(t *testing.T) {
				_, err := store.Apply(ctx, chronicle.ApplyRequest{
					Entity: chronicle.EntityRef{Kind: "employee", EntityID: "e1"},
					Plan:   chronicle.StaticWrite(chronicle.Write{}),
				})
				if err == nil {
					t.Fatal("Apply against a missing table succeeded")
				}
			})
		})
	})
}

// countRows reports how many rows the table holds, asked of SQL rather than of
// the library.
func countRows(ctx context.Context, t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	return n
}

// pad renders n as a fixed-width string, so that generated IDs sort in the
// order they were generated.
func pad(n int) string {
	s := make([]byte, 6)
	for i := 5; i >= 0; i-- {
		s[i] = byte('0' + n%10)
		n /= 10
	}
	return string(s)
}
