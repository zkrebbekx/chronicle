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
