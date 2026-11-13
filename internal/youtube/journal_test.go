package youtube

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestJournal_EmptyOrCorruptExistingFailsClosed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		content string
	}{
		{name: "empty file", content: ""},
		{name: "whitespace only", content: "   \n"},
		{name: "invalid json", content: "{corrupt"},
		{name: "wrong version", content: `{"version":99,"entries":{}}`},
		{name: "null entries", content: `{"version":1,"entries":null}`},
		{name: "mismatched key", content: `{"version":1,"entries":{"k1":{"key":"k2","state":"PENDING"}}}`},
		{name: "invalid state", content: `{"version":1,"entries":{"k1":{"key":"k1","state":"BOGUS"}}}`},
		{name: "completed missing video_id", content: `{"version":1,"entries":{"k1":{"key":"k1","state":"COMPLETED"}}}`},
		{name: "in_progress missing session_uri", content: `{"version":1,"entries":{"k1":{"key":"k1","state":"IN_PROGRESS"}}}`},
		{name: "negative sent_bytes", content: `{"version":1,"entries":{"k1":{"key":"k1","state":"PENDING","sent_bytes":-1}}}`},
		{name: "sent_bytes exceeds total_bytes", content: `{"version":1,"entries":{"k1":{"key":"k1","state":"PENDING","total_bytes":10,"sent_bytes":11}}}`},
		{name: "invalid file_sha256", content: `{"version":1,"entries":{"k1":{"key":"k1","state":"PENDING","file_sha256":"not-hex"}}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "journal.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenJournal(path); err == nil {
				t.Fatalf("expected OpenJournal to fail closed for %s", tc.name)
			}
		})
	}

	// Insecure 0644 permissions on existing journal must be rejected.
	path := filepath.Join(t.TempDir(), "journal.json")
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Put(context.Background(), JournalEntry{Key: "k1", State: UploadStatePending}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJournal(path); err == nil {
		t.Fatal("expected OpenJournal to reject journal file with mode 0644")
	}

	// Put must also reject invalid invariants before writing.
	j2, err := OpenJournal(filepath.Join(t.TempDir(), "j2.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := j2.Put(context.Background(), JournalEntry{Key: "k1", State: UploadStateInProgress}); err == nil {
		t.Fatal("expected Put to reject IN_PROGRESS without session_uri")
	}
}

func TestJournal_ContextCancellationWhenLockHeld(t *testing.T) {
	t.Parallel()

	j, err := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Already-cancelled context must fail immediately even when unlocked.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := j.AcquireKey(cancelledCtx, "k1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on AcquireKey, got %v", err)
	}
	if _, _, err := j.Get(cancelledCtx, "k1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on Get, got %v", err)
	}

	// Hold key lock and verify a second waiter cancels cleanly.
	release, err := j.AcquireKey(context.Background(), "busy-key")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer waitCancel()
	if _, err := j.AcquireKey(waitCtx, "busy-key"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded while lock held, got %v", err)
	}

	// Hold journal file lock and verify Put respects ctx cancellation.
	j.mu <- struct{}{}
	defer func() { <-j.mu }()
	putCtx, putCancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer putCancel()
	if err := j.Put(putCtx, JournalEntry{Key: "k2", State: UploadStatePending}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded on Put while lock held, got %v", err)
	}
}

func TestJournal_CrossProcessLockAndRecovery(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	journalPath := filepath.Join(dir, "journal.json")
	readyFile := filepath.Join(dir, "ready")

	j, err := OpenJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}

	// Spawn a real subprocess that acquires the key lock, writes an entry, and exits.
	cmd := exec.Command(os.Args[0], "-test.run=^TestJournal_HelperSubprocessLock$")
	cmd.Env = append(os.Environ(),
		"IT_NEWS_HELPER_JOURNAL="+journalPath,
		"IT_NEWS_HELPER_READY="+readyFile,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Wait for child process to signal it holds the lock.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(readyFile); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("helper subprocess did not signal readiness: %v", err)
	}

	// While child holds the lock, AcquireKey with short timeout must fail.
	shortCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := j.AcquireKey(shortCtx, "cross-proc-key"); !errors.Is(err, context.DeadlineExceeded) {
		_ = cmd.Wait()
		t.Fatalf("expected DeadlineExceeded while child holds lock, got %v", err)
	}

	// Wait for child to finish and release the lock, then acquire and read its entry.
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper subprocess failed: %v", err)
	}
	rel, err := j.AcquireKey(context.Background(), "cross-proc-key")
	if err != nil {
		t.Fatalf("AcquireKey after child exit failed: %v", err)
	}
	defer rel()

	entry, found, err := j.Get(context.Background(), "cross-proc-key")
	if err != nil || !found || entry.VideoID != "vid-from-child" {
		t.Fatalf("expected child entry, got found=%v entry=%+v err=%v", found, entry, err)
	}
}

func TestJournal_HelperSubprocessLock(t *testing.T) {
	journalPath := os.Getenv("IT_NEWS_HELPER_JOURNAL")
	readyFile := os.Getenv("IT_NEWS_HELPER_READY")
	if journalPath == "" || readyFile == "" {
		return
	}
	j, err := OpenJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := j.AcquireKey(context.Background(), "cross-proc-key")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	if err := os.WriteFile(readyFile, []byte("locked"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	if err := j.Put(context.Background(), JournalEntry{
		Key:     "cross-proc-key",
		State:   UploadStateCompleted,
		VideoID: "vid-from-child",
	}); err != nil {
		t.Fatal(err)
	}
}
