package chroniclefest_test

import (
	"testing"

	"github.com/zkrebbekx/chronicle"
	"github.com/zkrebbekx/chronicle/chroniclefest"
)

// TestMemStore runs the conformance suite against the reference
// implementation. It is what keeps the suite honest: a contract the reference
// store itself fails is a bug in the contract.
func TestMemStore(t *testing.T) {
	chroniclefest.Run(t, memStore)
}

func memStore(t chroniclefest.T) chronicle.Store {
	store := chronicle.NewMemStore()
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestMemStoreCompliance runs the deletion and legal-hold conformance suite
// against the reference implementation.
func TestMemStoreCompliance(t *testing.T) {
	chroniclefest.RunCompliance(t, memStore)
}

// TestMemKeyring runs the keyring conformance suite against the in-memory
// keyring.
func TestMemKeyring(t *testing.T) {
	chroniclefest.RunKeyring(t, func(chroniclefest.T) chronicle.Keyring {
		return chronicle.NewMemKeyring()
	})
}

// TestMemStoreHalves runs each half of the suite on its own. A store author who
// has not implemented the bitemporal engine's expectations yet still wants the
// store-level contract to be runnable, so the halves are public API and have to
// keep working independently.
func TestMemStoreHalves(t *testing.T) {
	t.Run("given the reference store", func(t *testing.T) {
		t.Run("when only the store contract is run", func(t *testing.T) {
			chroniclefest.RunStore(t, memStore)
		})
		t.Run("when only the log contract is run", func(t *testing.T) {
			chroniclefest.RunLog(t, memStore)
		})
	})
}
