package store

import (
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers pgx5://
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/sebasBetancourt/relay/migrations"
)

// Migrate applies every pending migration and returns the schema version. It
// keeps golang-migrate's schema_migrations table, so it shares a database with
// the Compose migrate service. The migrator takes an advisory lock, so two
// deploys cannot run it at once. A database left dirty by a failed migration is
// an error: repairing one needs a person.
func Migrate(databaseURL string) (uint, error) {
	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return 0, fmt.Errorf("migrate: source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", source, pgxURL(databaseURL))
	if err != nil {
		// Never wrap the URL: it holds the password. The driver's error does not.
		return 0, fmt.Errorf("migrate: connect: %w", err)
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return 0, fmt.Errorf("migrate: up: %w", err)
	}
	version, dirty, err := m.Version()
	if err != nil {
		return 0, fmt.Errorf("migrate: version: %w", err)
	}
	if dirty {
		return 0, fmt.Errorf("migrate: version %d is dirty", version)
	}
	return version, nil
}

// pgxURL swaps the scheme for the one the pgx driver registers.
func pgxURL(databaseURL string) string {
	_, rest, ok := strings.Cut(databaseURL, "://")
	if !ok {
		return databaseURL
	}
	return "pgx5://" + rest
}
