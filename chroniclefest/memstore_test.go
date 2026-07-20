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
	chroniclefest.Run(t, func(t *testing.T) chronicle.Store {
		store := chronicle.NewMemStore()
		t.Cleanup(func() { _ = store.Close() })
		return store
	})
}
