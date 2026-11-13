// Package postgres implements PostgreSQL persistence using pgxpool, raw SQL,
// and Goose migrations.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/migrations"
)

const defaultQueryTimeout = 5 * time.Second

// Connect creates and verifies a pgxpool.Pool using the provided configuration.
func Connect(ctx context.Context, cfg config.PostgresConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: invalid dsn %s", config.RedactDSN(cfg.DSN))
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns >= 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}

	timeout := cfg.QueryTimeout
	if timeout <= 0 {
		timeout = defaultQueryTimeout
	}
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping failed: %w", err)
	}
	return pool, nil
}

// Ping checks PostgreSQL connectivity with a bounded timeout.
func Ping(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration) error {
	if pool == nil {
		return fmt.Errorf("postgres: pool is nil")
	}
	if timeout <= 0 {
		timeout = defaultQueryTimeout
	}
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		return fmt.Errorf("postgres: ping: %w", err)
	}
	return nil
}

// MigrateUp applies all embedded SQL migrations to the target database.
func MigrateUp(ctx context.Context, dsn string) error {
	return runMigrations(ctx, dsn, func(p *goose.Provider) error {
		_, err := p.Up(ctx)
		return err
	})
}

// MigrateDown rolls back the most recent migration.
func MigrateDown(ctx context.Context, dsn string) error {
	return runMigrations(ctx, dsn, func(p *goose.Provider) error {
		_, err := p.Down(ctx)
		return err
	})
}

func runMigrations(ctx context.Context, dsn string, fn func(*goose.Provider) error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("postgres: migration cancelled: %w", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("postgres: open migration connection (%s): %w", config.RedactDSN(dsn), err)
	}
	defer db.Close()

	pingCtx, cancelPing := context.WithTimeout(ctx, defaultQueryTimeout)
	defer cancelPing()
	if err := db.PingContext(pingCtx); err != nil {
		return fmt.Errorf("postgres: ping migration connection (%s): %w", config.RedactDSN(dsn), err)
	}

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("postgres: create migration locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("postgres: create goose provider: %w", err)
	}
	if err := fn(provider); err != nil {
		return fmt.Errorf("postgres: run migrations: %w", err)
	}
	return nil
}

func withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = defaultQueryTimeout
	}
	return context.WithTimeout(ctx, timeout)
}
