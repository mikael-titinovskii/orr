package app

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mikael-titinovskii/orr/internal/store"
)

// openTestStore opens a fresh database in a temporary directory and closes it
// when the test ends.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "orr.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test store: %v", err)
		}
	})
	return db
}

// testRoutingWithStore returns a routing state that persists into its own
// database, along with that database for assertions.
func testRoutingWithStore(t *testing.T, models map[string]providerConfig) (*routingState, *store.Store) {
	t.Helper()
	db := openTestStore(t)
	routing := newRoutingState(models, time.Hour)
	routing.setPersistModels(db.SaveModels)
	return routing, db
}
