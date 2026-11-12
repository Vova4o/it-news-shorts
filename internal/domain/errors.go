package domain

import "errors"

// Sentinel errors of the domain layer. Callers use errors.Is to react to them.
var (
	ErrMissingField            = errors.New("domain: missing required field")
	ErrInvalidValue            = errors.New("domain: invalid value")
	ErrInvalidWindow           = errors.New("domain: invalid time window")
	ErrInvalidTransition       = errors.New("domain: invalid state transition")
	ErrSameState               = errors.New("domain: transition to the current state")
	ErrValidationRequired      = errors.New("domain: video job is not validated")
	ErrValidationFailed        = errors.New("domain: video validation failed")
	ErrPublicationNotAllowed   = errors.New("domain: publication is not allowed")
	ErrAlreadyPublished        = errors.New("domain: video job already has a publication")
	ErrRetriesExhausted        = errors.New("domain: retry attempts exhausted")
	ErrDuplicateID             = errors.New("domain: duplicate identifier")
	ErrDuplicateIdempotencyKey = errors.New("domain: duplicate idempotency key")
	ErrDurationOutOfRange      = errors.New("domain: shorts duration out of range")
	ErrBudgetExceeded          = errors.New("domain: generation budget exceeded")
	ErrTooManyStories          = errors.New("domain: too many stories requested")
)

// ValidationError points at the field that broke a domain rule.
type ValidationError struct {
	Field string
	Err   error
}

func (e *ValidationError) Error() string {
	return "domain: field " + e.Field + ": " + e.Err.Error()
}

// Unwrap keeps errors.Is(err, ErrMissingField) working for every model.
func (e *ValidationError) Unwrap() error { return e.Err }

// MissingField reports an absent required field.
func MissingField(field string) error {
	return &ValidationError{Field: field, Err: ErrMissingField}
}

// InvalidValue reports a field that is present but violates a rule.
func InvalidValue(field string) error {
	return &ValidationError{Field: field, Err: ErrInvalidValue}
}

func missing(field string) error { return MissingField(field) }

func invalid(field string) error { return InvalidValue(field) }
