package pgstore_test

import (
	"testing"

	"github.com/zkrebbekx/chronicle"
	"github.com/zkrebbekx/chronicle/chroniclefest"
)

// TestConformance runs the shared store contract against Postgres. The same
// suite runs against MemStore in the root module, which is the mechanism that
// keeps the two implementations answering identically rather than merely
// plausibly — same ordering, same tie handling, same page boundaries, same
// treatment of half-open bounds.
func TestConformance(t *testing.T) {
	db := testDB(t)
	chroniclefest.Run(t, func(t chroniclefest.T) chronicle.Store {
		return newStore(t, db)
	})
}

// TestComplianceConformance runs the deletion and legal-hold contract against
// Postgres, mirroring the MemStore run in the root module.
func TestComplianceConformance(t *testing.T) {
	db := testDB(t)
	chroniclefest.RunCompliance(t, func(t chroniclefest.T) chronicle.Store {
		return newStore(t, db)
	})
}

// TestKeyringConformance runs the keyring contract against the Postgres
// keyring, one schema per test.
func TestKeyringConformance(t *testing.T) {
	db := testDB(t)
	chroniclefest.RunKeyring(t, func(t chroniclefest.T) chronicle.Keyring {
		return newKeyring(t, db)
	})
}
