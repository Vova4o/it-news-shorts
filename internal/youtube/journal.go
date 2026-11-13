package youtube

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

// JournalEntry is one recorded upload operation.
type JournalEntry struct {
	Key           string      `json:"key"`
	State         UploadState `json:"state"`
	VideoID       string      `json:"video_id,omitempty"`
	UploadStatus  string      `json:"upload_status,omitempty"`
	SessionURI    string      `json:"session_uri,omitempty"`
	TotalBytes    int64       `json:"total_bytes,omitempty"`
	SentBytes     int64       `json:"sent_bytes,omitempty"`
	FileSHA256    string      `json:"file_sha256,omitempty"`
	Title         string      `json:"title,omitempty"`
	Privacy       string      `json:"privacy,omitempty"`
	VideoJobID    string      `json:"video_job_id,omitempty"`
	PublicationID string      `json:"publication_id,omitempty"`
	Error         string      `json:"error,omitempty"`
	UploadedAt    time.Time   `json:"uploaded_at,omitempty"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

type journalFile struct {
	Version int                     `json:"version"`
	Entries map[string]JournalEntry `json:"entries"`
}

const journalVersion = 1

// UploadJournal is the consumer-owned persistence contract for idempotent
// YouTube upload state. Both the local file Journal and PostgreSQL journal
// implement this interface.
type UploadJournal interface {
	AcquireKey(ctx context.Context, key string) (func(), error)
	Get(ctx context.Context, key string) (JournalEntry, bool, error)
	FindByVideoID(ctx context.Context, videoID string) (JournalEntry, bool, error)
	Put(ctx context.Context, entry JournalEntry) error
	List(ctx context.Context) ([]JournalEntry, error)
}

// Journal is a durable local record of upload operations keyed by our own
// idempotency key. YouTube does not deduplicate videos.insert by such a key,
// so this journal is what prevents a second video on a repeated call.
//
// The file-based journal lives in one JSON file that is replaced atomically and
// protected by advisory file locks across processes.
type Journal struct {
	path string
	mu   chan struct{}
	now  func() time.Time
}

// OpenJournal opens or creates the journal at path. The parent directory is
// created with 0700 and the file with 0600, because entries may contain
// resumable session URIs. An existing empty, truncated, or corrupt journal
// fails closed instead of silently resetting idempotency state.
func OpenJournal(path string) (*Journal, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("youtube: journal path is required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("youtube: create journal directory: %w", err)
	}
	journal := &Journal{
		path: path,
		mu:   make(chan struct{}, 1),
		now:  time.Now,
	}
	if _, err := os.Stat(path); err == nil {
		if _, err := journal.read(); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("youtube: stat journal: %w", err)
	}
	return journal, nil
}

// Path returns the journal file path.
func (j *Journal) Path() string { return j.path }

// Get returns the entry for key.
func (j *Journal) Get(ctx context.Context, key string) (JournalEntry, bool, error) {
	var entry JournalEntry
	var ok bool
	err := j.withFileLock(ctx, func() error {
		file, err := j.read()
		if err != nil {
			return err
		}
		entry, ok = file.Entries[key]
		return nil
	})
	return entry, ok, err
}

// FindByVideoID returns the first entry matching videoID.
func (j *Journal) FindByVideoID(ctx context.Context, videoID string) (JournalEntry, bool, error) {
	var match JournalEntry
	var found bool
	err := j.withFileLock(ctx, func() error {
		file, err := j.read()
		if err != nil {
			return err
		}
		for _, entry := range file.Entries {
			if entry.VideoID == videoID {
				match = entry
				found = true
				return nil
			}
		}
		return nil
	})
	return match, found, err
}

// Put stores the entry, stamping UpdatedAt and CreatedAt.
func (j *Journal) Put(ctx context.Context, entry JournalEntry) error {
	if err := ValidateJournalEntry(entry); err != nil {
		return err
	}
	return j.withFileLock(ctx, func() error {
		file, err := j.read()
		if err != nil {
			return err
		}
		now := j.now().UTC()
		if previous, ok := file.Entries[entry.Key]; ok && !previous.CreatedAt.IsZero() {
			entry.CreatedAt = previous.CreatedAt
		} else {
			entry.CreatedAt = now
		}
		entry.UpdatedAt = now
		file.Entries[entry.Key] = entry
		return j.write(file)
	})
}

// List returns all entries ordered by creation time, newest first.
func (j *Journal) List(ctx context.Context) ([]JournalEntry, error) {
	var entries []JournalEntry
	err := j.withFileLock(ctx, func() error {
		file, err := j.read()
		if err != nil {
			return err
		}
		entries = make([]JournalEntry, 0, len(file.Entries))
		for _, entry := range file.Entries {
			entries = append(entries, entry)
		}
		sort.Slice(entries, func(a, b int) bool {
			return entries[a].CreatedAt.After(entries[b].CreatedAt)
		})
		return nil
	})
	return entries, err
}

// AcquireKey acquires an exclusive cross-process advisory lock for one
// idempotency key, respecting ctx cancellation. Callers must invoke the
// returned release function when the operation finishes.
func (j *Journal) AcquireKey(ctx context.Context, key string) (func(), error) {
	if key == "" {
		return nil, fmt.Errorf("youtube: journal entry key is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lockDir := filepath.Join(filepath.Dir(j.path), ".locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("youtube: create lock directory: %w", err)
	}
	sum := sha256.Sum256([]byte(key))
	lockPath := filepath.Join(lockDir, hex.EncodeToString(sum[:16])+".lock")
	return acquireFlock(ctx, lockPath)
}

func (j *Journal) withFileLock(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case j.mu <- struct{}{}:
		defer func() { <-j.mu }()
	case <-ctx.Done():
		return ctx.Err()
	}

	if err := os.MkdirAll(filepath.Dir(j.path), 0o700); err != nil {
		return fmt.Errorf("youtube: create journal directory: %w", err)
	}
	release, err := acquireFlock(ctx, j.path+".lock")
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

func acquireFlock(ctx context.Context, lockPath string) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("youtube: open lock %s: %w", filepath.Base(lockPath), err)
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		_ = f.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("youtube: lock %s: %w", filepath.Base(lockPath), err)
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (j *Journal) read() (journalFile, error) {
	file := journalFile{Version: journalVersion, Entries: map[string]JournalEntry{}}
	info, err := os.Lstat(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return file, nil
		}
		return journalFile{}, fmt.Errorf("youtube: stat journal: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return journalFile{}, fmt.Errorf("youtube: journal file %s must not be a symlink", j.path)
	}
	if !info.Mode().IsRegular() {
		return journalFile{}, fmt.Errorf("youtube: journal file %s must be a regular file", j.path)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		return journalFile{}, fmt.Errorf("youtube: journal file %s has insecure permissions %04o (want 0600)", j.path, perm)
	}

	raw, err := os.ReadFile(j.path)
	if err != nil {
		return journalFile{}, fmt.Errorf("youtube: read journal: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return journalFile{}, fmt.Errorf("youtube: journal file %s is empty or truncated", j.path)
	}
	var parsed journalFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return journalFile{}, fmt.Errorf("youtube: parse journal: %w", err)
	}
	if parsed.Version != journalVersion {
		return journalFile{}, fmt.Errorf("youtube: unsupported journal version %d (want %d)", parsed.Version, journalVersion)
	}
	if parsed.Entries == nil {
		return journalFile{}, fmt.Errorf("youtube: journal file %s has null or missing entries", j.path)
	}
	for k, entry := range parsed.Entries {
		if err := ValidateJournalEntryWithKey(k, entry); err != nil {
			return journalFile{}, err
		}
	}
	return parsed, nil
}

// ValidateJournalEntry enforces all consumer-owned upload journal invariants
// across file and database implementations.
func ValidateJournalEntry(entry JournalEntry) error {
	return ValidateJournalEntryWithKey(entry.Key, entry)
}

// ValidateJournalEntryWithKey validates an upload journal entry against its
// expected map/storage key.
func ValidateJournalEntryWithKey(key string, entry JournalEntry) error {
	if strings.TrimSpace(key) == "" || entry.Key != key {
		return fmt.Errorf("youtube: journal entry %q has mismatched or empty key %q", key, entry.Key)
	}
	if !validUploadState(entry.State) {
		return fmt.Errorf("youtube: journal entry %q has invalid state %q", key, entry.State)
	}
	if entry.TotalBytes < 0 || entry.SentBytes < 0 {
		return fmt.Errorf("youtube: journal entry %q has negative byte count", key)
	}
	if entry.TotalBytes > 0 && entry.SentBytes > entry.TotalBytes {
		return fmt.Errorf("youtube: journal entry %q has sent_bytes %d > total_bytes %d", key, entry.SentBytes, entry.TotalBytes)
	}
	if entry.FileSHA256 != "" {
		if len(entry.FileSHA256) != 64 {
			return fmt.Errorf("youtube: journal entry %q has invalid file_sha256 length", key)
		}
		if _, err := hex.DecodeString(entry.FileSHA256); err != nil {
			return fmt.Errorf("youtube: journal entry %q has invalid file_sha256 hex", key)
		}
	}
	if entry.State == UploadStateInProgress && strings.TrimSpace(entry.SessionURI) == "" {
		return fmt.Errorf("youtube: in-progress journal entry %q requires session_uri", key)
	}
	if entry.State == UploadStateCompleted && strings.TrimSpace(entry.VideoID) == "" {
		return fmt.Errorf("youtube: completed journal entry %q is missing video_id", key)
	}
	return nil
}

func validUploadState(state UploadState) bool {
	switch state {
	case UploadStatePending, UploadStateInProgress, UploadStateCompleted, UploadStateFailed, UploadStateUnknown:
		return true
	default:
		return false
	}
}

func (j *Journal) write(file journalFile) error {
	file.Version = journalVersion
	if file.Entries == nil {
		file.Entries = map[string]JournalEntry{}
	}
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("youtube: encode journal: %w", err)
	}
	return writeFileAtomic(j.path, raw, 0o600)
}

// writeFileAtomic writes data to a temporary file in the same directory,
// fsyncs it, renames it over path, and syncs the parent directory.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("youtube: create temporary file: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name)

	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return fmt.Errorf("youtube: set file mode: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("youtube: write temporary file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("youtube: sync temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("youtube: close temporary file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("youtube: replace file: %w", err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("youtube: open directory for sync: %w", err)
	}
	defer d.Close()

	if err := d.Sync(); err != nil {
		if runtime.GOOS != "windows" && (errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)) {
			return nil
		}
		return fmt.Errorf("youtube: sync directory: %w", err)
	}
	return nil
}
