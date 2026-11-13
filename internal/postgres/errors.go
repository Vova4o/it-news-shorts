package postgres

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Repository sentinel errors.
var (
	ErrNotFound               = errors.New("postgres: record not found")
	ErrConflict               = errors.New("postgres: unique constraint conflict")
	ErrIdempotencyConflict    = errors.New("postgres: idempotency conflict with existing record")
	ErrForeignKey             = errors.New("postgres: foreign key violation")
	ErrConcurrentModification = errors.New("postgres: concurrent modification detected")
)

func mapPGError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", op, ErrNotFound)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return fmt.Errorf("%s: %w (%s)", op, ErrConflict, pgErr.ConstraintName)
		case "23503":
			return fmt.Errorf("%s: %w (%s)", op, ErrForeignKey, pgErr.ConstraintName)
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}
