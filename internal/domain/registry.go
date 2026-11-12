package domain

import "fmt"

// Registry detects duplicate identifiers and duplicate idempotency keys inside
// one process. It is the in-memory expression of the database unique
// constraints that arrive in a later stage.
type Registry struct {
	ids  map[string]struct{}
	keys map[string]struct{}
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		ids:  make(map[string]struct{}),
		keys: make(map[string]struct{}),
	}
}

// RegisterID records an identifier of the given kind and fails on duplicates.
func (r *Registry) RegisterID(kind, id string) error {
	if kind == "" || id == "" {
		return missing("registry.id")
	}
	if r.ids == nil {
		r.ids = make(map[string]struct{})
	}
	key := kind + "/" + id
	if _, ok := r.ids[key]; ok {
		return fmt.Errorf("%w: %s", ErrDuplicateID, key)
	}
	r.ids[key] = struct{}{}
	return nil
}

// HasID reports whether the identifier was registered.
func (r *Registry) HasID(kind, id string) bool {
	_, ok := r.ids[kind+"/"+id]
	return ok
}

// RegisterIdempotencyKey records a key and fails on duplicates.
func (r *Registry) RegisterIdempotencyKey(key IdempotencyKey) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if r.keys == nil {
		r.keys = make(map[string]struct{})
	}
	if _, ok := r.keys[string(key)]; ok {
		return fmt.Errorf("%w: %s", ErrDuplicateIdempotencyKey, key)
	}
	r.keys[string(key)] = struct{}{}
	return nil
}

// HasIdempotencyKey reports whether the key was registered.
func (r *Registry) HasIdempotencyKey(key IdempotencyKey) bool {
	_, ok := r.keys[string(key)]
	return ok
}
