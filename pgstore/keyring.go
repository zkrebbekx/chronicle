package pgstore

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/zkrebbekx/chronicle"
)

//go:embed keys.sql
var keysSQL string

// DefaultKeysTable is the keyring's table name when none is configured.
const DefaultKeysTable = "chronicle_keys"

// Keyring is a [chronicle.Keyring] backed by one PostgreSQL table.
//
// Understand what this placement buys and what it costs before relying on it
// for shredding. Keys in the same database as the records mean one operational
// story — one backup, one connection, one migration path — and they mean a
// database administrator holds both the ciphertext and the key, and that
// every backup of the keys table keeps destroyed keys restorable for as long
// as the backup exists. Crypto-shredding is exactly as strong as the
// destruction of every copy of the key. A deployment that needs shredding to
// withstand its own administrators or its own backup retention should back
// [chronicle.Keyring] with a KMS or HSM instead; the interface is the whole
// of what chronicle requires.
type Keyring struct {
	db        *sql.DB
	schema    string
	table     string
	qualified string
}

// NewKeyring returns a keyring over db. Like [New] it does not touch the
// database; call [Keyring.Migrate] or apply [KeysSchemaSQL] through your own
// migration tool first. [WithSchema] and [WithTable] apply, with
// [DefaultKeysTable] as the default table name.
func NewKeyring(db *sql.DB, opts ...Option) (*Keyring, error) {
	if db == nil {
		return nil, errors.New("pgstore: NewKeyring requires a non-nil *sql.DB")
	}
	cfg := config{table: DefaultKeysTable}
	for _, opt := range opts {
		opt(&cfg)
	}
	if err := validIdentifier("table", cfg.table); err != nil {
		return nil, err
	}
	if cfg.schema != "" {
		if err := validIdentifier("schema", cfg.schema); err != nil {
			return nil, err
		}
	}
	return &Keyring{db: db, schema: cfg.schema, table: cfg.table, qualified: qualify(cfg.schema, cfg.table)}, nil
}

// Table returns the quoted, schema-qualified keys table name, mirroring
// [Store.Table]. Intended for diagnostics and for tests that need to reach
// past the library.
func (k *Keyring) Table() string { return k.qualified }

// Migrate creates the keys table if it is not already there. It is idempotent
// and safe to run on every boot.
func (k *Keyring) Migrate(ctx context.Context) error {
	sqlText, err := KeysSchemaSQL(k.schema, k.table)
	if err != nil {
		return err
	}
	if _, err := k.db.ExecContext(ctx, sqlText); err != nil {
		return fmt.Errorf("pgstore: migrate %s: %w", k.qualified, err)
	}
	return nil
}

// KeysSchemaSQL returns the keyring's DDL for the given names, for callers
// who feed migrations to their own tooling. An empty table means
// [DefaultKeysTable].
func KeysSchemaSQL(schema, table string) (string, error) {
	if table == "" {
		table = DefaultKeysTable
	}
	if err := validIdentifier("table", table); err != nil {
		return "", err
	}
	if schema != "" {
		if err := validIdentifier("schema", schema); err != nil {
			return "", err
		}
	}
	out := strings.ReplaceAll(keysSQL, "$TABLE$", qualify(schema, table))
	return strings.ReplaceAll(out, "$NAME$", table), nil
}

// Key implements [chronicle.Keyring]. The mint is race-safe without a
// transaction: both racers INSERT with ON CONFLICT DO NOTHING and both then
// read the single surviving row, so no writer can observe a key the other
// writer does not.
func (k *Keyring) Key(ctx context.Context, subject string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if subject == "" {
		return nil, &chronicle.KeyError{Err: errors.New("subject is empty")}
	}

	key, found, err := k.lookup(ctx, subject)
	if err != nil || found {
		return key, err
	}

	fresh := make([]byte, chronicle.KeySize)
	if _, err := cryptorand.Read(fresh); err != nil {
		return nil, &chronicle.KeyError{Subject: subject, Err: err}
	}
	if _, err := k.db.ExecContext(ctx,
		`INSERT INTO `+k.qualified+` (subject, key) VALUES ($1, $2) ON CONFLICT (subject) DO NOTHING`,
		subject, fresh); err != nil {
		return nil, &chronicle.KeyError{Subject: subject, Err: err}
	}

	key, found, err = k.lookup(ctx, subject)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, &chronicle.KeyError{Subject: subject, Err: errors.New("key vanished between insert and read")}
	}
	return key, nil
}

// lookup reads a subject's row, translating a destroyed subject into
// [chronicle.ErrKeyDestroyed].
func (k *Keyring) lookup(ctx context.Context, subject string) (key []byte, found bool, err error) {
	var (
		stored    []byte
		destroyed bool
	)
	err = k.db.QueryRowContext(ctx,
		`SELECT key, destroyed_at IS NOT NULL FROM `+k.qualified+` WHERE subject = $1`,
		subject).Scan(&stored, &destroyed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, &chronicle.KeyError{Subject: subject, Err: err}
	}
	if destroyed {
		return nil, false, &chronicle.KeyError{Subject: subject, Err: chronicle.ErrKeyDestroyed}
	}
	if len(stored) != chronicle.KeySize {
		return nil, false, &chronicle.KeyError{Subject: subject,
			Err: fmt.Errorf("stored key has %d bytes; want %d", len(stored), chronicle.KeySize)}
	}
	return stored, true, nil
}

// DestroyKey implements [chronicle.Keyring]. The row survives with the key
// nulled, which is the marker that keeps destruction terminal; a subject that
// never had a key gets the marker directly. COALESCE keeps the first
// destruction's timestamp across repeats.
func (k *Keyring) DestroyKey(ctx context.Context, subject string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if subject == "" {
		return &chronicle.KeyError{Err: errors.New("subject is empty")}
	}
	if _, err := k.db.ExecContext(ctx,
		`INSERT INTO `+k.qualified+` AS keys (subject, key, destroyed_at) VALUES ($1, NULL, clock_timestamp())`+
			` ON CONFLICT (subject) DO UPDATE SET key = NULL,`+
			` destroyed_at = COALESCE(keys.destroyed_at, clock_timestamp())`,
		subject); err != nil {
		return &chronicle.KeyError{Subject: subject, Err: err}
	}
	return nil
}

// Compile-time assertion.
var _ chronicle.Keyring = (*Keyring)(nil)
