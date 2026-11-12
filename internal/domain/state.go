package domain

import "fmt"

// VideoState is the lifecycle state of a VideoJob.
type VideoState string

// Lifecycle states. The happy path is CREATED -> SCRIPTED -> MEDIA_READY ->
// RENDERED -> VALIDATED -> UPLOADED -> SCHEDULED -> PUBLISHED.
const (
	StateCreated      VideoState = "CREATED"
	StateScripted     VideoState = "SCRIPTED"
	StateMediaReady   VideoState = "MEDIA_READY"
	StateRendered     VideoState = "RENDERED"
	StateValidated    VideoState = "VALIDATED"
	StateUploaded     VideoState = "UPLOADED"
	StateScheduled    VideoState = "SCHEDULED"
	StatePublished    VideoState = "PUBLISHED"
	StateFailed       VideoState = "FAILED"
	StateRejected     VideoState = "REJECTED"
	StateRetryPending VideoState = "RETRY_PENDING"
)

func (s VideoState) String() string { return string(s) }

// Valid reports whether the state is known.
func (s VideoState) Valid() bool {
	switch s {
	case StateCreated, StateScripted, StateMediaReady, StateRendered, StateValidated,
		StateUploaded, StateScheduled, StatePublished, StateFailed, StateRejected,
		StateRetryPending:
		return true
	default:
		return false
	}
}

// Terminal reports whether no further transition is possible.
func (s VideoState) Terminal() bool {
	return s == StatePublished || s == StateRejected
}

func targets(states ...VideoState) map[VideoState]struct{} {
	set := make(map[VideoState]struct{}, len(states))
	for _, state := range states {
		set[state] = struct{}{}
	}
	return set
}

// transitions lists the states a job may move to. RETRY_PENDING has no static
// targets: it may only return to the state recorded as the resume state.
var transitions = map[VideoState]map[VideoState]struct{}{
	StateCreated:      targets(StateScripted, StateFailed, StateRejected, StateRetryPending),
	StateScripted:     targets(StateMediaReady, StateFailed, StateRejected, StateRetryPending),
	StateMediaReady:   targets(StateRendered, StateFailed, StateRejected, StateRetryPending),
	StateRendered:     targets(StateValidated, StateFailed, StateRejected, StateRetryPending),
	StateValidated:    targets(StateUploaded, StateFailed, StateRejected, StateRetryPending),
	StateUploaded:     targets(StateScheduled, StateFailed, StateRetryPending),
	StateScheduled:    targets(StatePublished, StateFailed, StateRetryPending),
	StateFailed:       targets(StateRetryPending, StateRejected),
	StateRetryPending: targets(),
	StatePublished:    targets(),
	StateRejected:     targets(),
}

func allowedTargets(from VideoState) map[VideoState]struct{} {
	return transitions[from]
}

// CanTransition reports whether from -> to is allowed without any extra
// precondition.
func CanTransition(from, to VideoState) bool {
	if from == to {
		return false
	}
	_, ok := allowedTargets(from)[to]
	return ok
}

// ValidateTransition checks the static part of the state machine.
func ValidateTransition(from, to VideoState) error {
	if from == to {
		return fmt.Errorf("%w: %s", ErrSameState, from)
	}
	if !from.Valid() {
		return fmt.Errorf("%w: unknown source state %q", ErrInvalidTransition, from)
	}
	if !to.Valid() {
		return fmt.Errorf("%w: unknown target state %q", ErrInvalidTransition, to)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	return nil
}

// retryable reports whether a failure in this state can be retried from it.
func retryable(state VideoState) bool {
	switch state {
	case StateCreated, StateScripted, StateMediaReady, StateRendered, StateValidated, StateUploaded:
		return true
	default:
		return false
	}
}
