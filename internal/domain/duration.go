package domain

import "time"

// Bounds of a YouTube Short produced by the service.
const (
	MinShortsDuration = 15 * time.Second
	MaxShortsDuration = 30 * time.Second
)

// ShortsDuration is a duration that is guaranteed to be inside the 15-30
// second window.
type ShortsDuration time.Duration

// NewShortsDuration validates the duration bounds.
func NewShortsDuration(d time.Duration) (ShortsDuration, error) {
	if d < MinShortsDuration || d > MaxShortsDuration {
		return 0, &ValidationError{Field: "duration", Err: ErrDurationOutOfRange}
	}
	return ShortsDuration(d), nil
}

// Duration returns the standard library duration.
func (d ShortsDuration) Duration() time.Duration { return time.Duration(d) }

// Seconds returns the duration in seconds.
func (d ShortsDuration) Seconds() float64 { return time.Duration(d).Seconds() }

// Validate reports whether the duration is inside the allowed window.
func (d ShortsDuration) Validate() error {
	_, err := NewShortsDuration(time.Duration(d))
	return err
}
