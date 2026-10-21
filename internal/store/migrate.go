package store

import (
	"database/sql"
	"embed"
	"fmt"
	"sync"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

// goose keeps its dialect and filesystem in package-level state, so they are
// configured once and reused by every Open in the process.
var gooseOnce sync.Once
var gooseErr error

func configureGoose() error {
	gooseOnce.Do(func() {
		goose.SetBaseFS(migrations)
		goose.SetLogger(goose.NopLogger())
		gooseErr = goose.SetDialect("sqlite3")
	})
	return gooseErr
}

func migrate(db *sql.DB) error {
	if err := configureGoose(); err != nil {
		return fmt.Errorf("configure migrations: %w", err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
