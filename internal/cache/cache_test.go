package cache_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jesusrobot0/clauchy/internal/cache"
)

func TestReadWriteRoundTrip(t *testing.T) {
	t.Parallel()

	c := cache.New(t.TempDir())
	payload := []byte("hello, cache!")

	if err := c.Write("test.json", payload); err != nil {
		t.Fatalf("Write() error: %v", err)
	}

	got, err := c.Read("test.json")
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}

	if string(got) != string(payload) {
		t.Errorf("Read() = %q, want %q", got, payload)
	}
}

func TestReadMissingKey(t *testing.T) {
	t.Parallel()

	c := cache.New(t.TempDir())
	_, err := c.Read("nonexistent.json")
	if !errors.Is(err, cache.ErrMiss) {
		t.Errorf("Read() error = %v, want ErrMiss", err)
	}
}

func TestWriteAtomicity(t *testing.T) {
	t.Parallel()

	// Write payload A, then overwrite with payload B.
	// A Read after the second Write must return B exactly — never a mix.
	c := cache.New(t.TempDir())

	payloadA := []byte("version-A: " + string(make([]byte, 1024))) // non-trivial size
	payloadB := []byte("version-B: " + string(make([]byte, 1024)))

	if err := c.Write("data.json", payloadA); err != nil {
		t.Fatalf("first Write() error: %v", err)
	}
	if err := c.Write("data.json", payloadB); err != nil {
		t.Fatalf("second Write() error: %v", err)
	}

	got, err := c.Read("data.json")
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if string(got) != string(payloadB) {
		t.Errorf("Read() after overwrite = %q…, want %q…", got[:20], payloadB[:20])
	}
}

func TestWithLockSerializesGoroutines(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrent lock serialization test in short mode")
	}

	c := cache.New(t.TempDir())

	var mu sync.Mutex
	var seq []int

	var wg sync.WaitGroup
	wg.Add(2)

	started := make(chan struct{})

	// Goroutine 1: acquires the lock, records 1, sleeps 60ms, records 2.
	go func() {
		defer wg.Done()
		if err := c.WithLock(".test.lock", 3*time.Second, func() error {
			mu.Lock()
			seq = append(seq, 1)
			mu.Unlock()
			close(started)
			time.Sleep(60 * time.Millisecond)
			mu.Lock()
			seq = append(seq, 2)
			mu.Unlock()
			return nil
		}); err != nil {
			t.Errorf("goroutine 1 WithLock: %v", err)
		}
	}()

	// Goroutine 2: waits until goroutine 1 has the lock, then contends.
	go func() {
		defer wg.Done()
		<-started
		if err := c.WithLock(".test.lock", 3*time.Second, func() error {
			mu.Lock()
			seq = append(seq, 3)
			mu.Unlock()
			return nil
		}); err != nil {
			t.Errorf("goroutine 2 WithLock: %v", err)
		}
	}()

	wg.Wait()

	if len(seq) != 3 || seq[0] != 1 || seq[1] != 2 || seq[2] != 3 {
		t.Errorf("WithLock did not serialize goroutines: got %v, want [1 2 3]", seq)
	}
}

// ─── Fix 6b: cache dir 0700, lock files 0600 ──────────────────────────────────

func TestWrite_DirMode0700(t *testing.T) {
	t.Parallel()

	// Use a subdirectory so we can check its mode after Write creates it.
	parent := t.TempDir()
	dir := filepath.Join(parent, "clauchy-cache")
	c := cache.New(dir)

	if err := c.Write("test.json", []byte(`{}`)); err != nil {
		t.Fatalf("Write() error: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("cache dir mode = 0%o, want 0700", info.Mode().Perm())
	}
}

func TestWithLock_LockFileMode0600(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	dir := filepath.Join(parent, "clauchy-lock")
	c := cache.New(dir)

	var lockFilePath string
	_ = c.WithLock(".test.lock", 3*time.Second, func() error {
		lockFilePath = filepath.Join(dir, ".test.lock")
		return nil
	})

	if lockFilePath == "" {
		t.Fatal("lock fn was not called")
	}
	info, err := os.Stat(lockFilePath)
	if err != nil {
		t.Fatalf("Stat(%q): %v", lockFilePath, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("lock file mode = 0%o, want 0600", info.Mode().Perm())
	}
}

func TestWithLockErrLockTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping lock timeout test in short mode")
	}

	c := cache.New(t.TempDir())

	locked := make(chan struct{})
	released := make(chan struct{})

	// Hold the lock for 300ms.
	go func() {
		_ = c.WithLock(".timeout.lock", 3*time.Second, func() error {
			close(locked)
			time.Sleep(300 * time.Millisecond)
			return nil
		})
		close(released)
	}()

	// Wait until the goroutine holds the lock.
	<-locked

	// Try to acquire with a 50ms timeout — must return ErrLockTimeout.
	err := c.WithLock(".timeout.lock", 50*time.Millisecond, func() error {
		t.Error("lock fn should not have been called")
		return nil
	})

	if !errors.Is(err, cache.ErrLockTimeout) {
		t.Errorf("WithLock() error = %v, want ErrLockTimeout", err)
	}

	<-released
}
