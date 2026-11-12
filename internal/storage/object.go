package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

// Storage errors.
var (
	ErrMissingBucket = errors.New("storage: missing bucket")
	ErrMissingKey    = errors.New("storage: missing object key")
	ErrNegativeSize  = errors.New("storage: negative object size")
)

// Object describes an object to write.
type Object struct {
	Bucket      string
	Key         string
	ContentType string
	Size        int64
}

// Validate enforces the object rules.
func (o Object) Validate() error {
	if strings.TrimSpace(o.Bucket) == "" {
		return ErrMissingBucket
	}
	if strings.TrimSpace(o.Key) == "" {
		return ErrMissingKey
	}
	if o.Size < 0 {
		return ErrNegativeSize
	}
	return nil
}

// ObjectRef points to a stored object.
type ObjectRef struct {
	Bucket string
	Key    string
	ETag   string
}

// ObjectStorage is the MinIO compatible storage port.
type ObjectStorage interface {
	Put(ctx context.Context, obj Object, r io.Reader) (ObjectRef, error)
	Get(ctx context.Context, ref ObjectRef) (io.ReadCloser, error)
	Delete(ctx context.Context, ref ObjectRef) error
	PresignGet(ctx context.Context, ref ObjectRef, ttl time.Duration) (string, error)
}
