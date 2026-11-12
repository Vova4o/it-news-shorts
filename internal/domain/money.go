package domain

import "fmt"

// MicroUSD stores money as integer micro-dollars to avoid float rounding.
type MicroUSD int64

// NewMicroUSD validates a non-negative amount.
func NewMicroUSD(v int64) (MicroUSD, error) {
	if v < 0 {
		return 0, invalid("amount")
	}
	return MicroUSD(v), nil
}

// Int64 returns the raw micro-dollar amount.
func (m MicroUSD) Int64() int64 { return int64(m) }

// Validate reports whether the amount is non-negative.
func (m MicroUSD) Validate() error {
	if m < 0 {
		return invalid("amount")
	}
	return nil
}

func (m MicroUSD) String() string {
	units := int64(m) / 1_000_000
	fraction := int64(m) % 1_000_000
	if fraction < 0 {
		fraction = -fraction
	}
	return fmt.Sprintf("%d.%06d", units, fraction)
}

// Add returns the sum of two amounts.
func (m MicroUSD) Add(other MicroUSD) MicroUSD { return m + other }
