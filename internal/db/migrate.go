package db

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	migrations "github.com/cshubhamrao/cloud-credit-system/sql/migrations"
)

// RunMigrations applies all SQL migrations in sql/migrations/ in sorted order.
// The migration files use CREATE ... IF NOT EXISTS so re-running is safe.
func RunMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn for migrations: %w", err)
	}
	defer conn.Release()

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := migrations.FS.ReadFile(e.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		slog.Info("applying migration", "file", e.Name())
		if _, err := conn.Exec(ctx, string(data)); err != nil {
			return fmt.Errorf("apply migration %s: %w", e.Name(), err)
		}
	}
	return nil
}
