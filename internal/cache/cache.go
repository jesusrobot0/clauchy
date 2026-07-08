// Package cache provides a generic, flock-guarded, atomic-write disk cache.
// It stores and retrieves raw bytes by name within a single directory.
//
// Key design decisions:
//   - Writes use os.CreateTemp + os.Rename (same directory) for atomic visibility.
//   - WithLock uses a non-blocking flock poll loop bounded by a deadline so
//     contention never hangs the caller.
//   - Freshness is NOT interpreted here; callers embed a cachedAt timestamp in
//     the payload and decide staleness themselves (ADR-11).
package cache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrMiss is returned by Read when the named entry does not exist in the cache.
var ErrMiss = errors.New("cache: miss")

// ErrLockTimeout is returned by WithLock when the exclusive flock could not be
// acquired within the requested timeout.
var ErrLockTimeout = errors.New("cache: lock timeout")

// Cache is a directory-backed byte store. Create one with New.
type Cache struct {
	dir string
}

// New returns a Cache that stores files under dir. The directory is created on
// the first Write if it does not exist.
func New(dir string) *Cache {
	return &Cache{dir: dir}
}

// Read returns the raw bytes stored under name, or ErrMiss if the entry does
// not exist. Other I/O errors are wrapped and returned as-is.
func (c *Cache) Read(name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(c.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrMiss
	}
	if err != nil {
		return nil, fmt.Errorf("cache read %s: %w", name, err)
	}
	return data, nil
}

// Write atomically replaces (or creates) the named entry with b.
// It creates a temp file in the same directory as the target so that
// os.Rename is guaranteed to be atomic (same filesystem, no cross-device move).
func (c *Cache) Write(name string, b []byte) error {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return fmt.Errorf("cache mkdir: %w", err)
	}

	tmp, err := os.CreateTemp(c.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("cache create temp: %w", err)
	}
	tmpName := tmp.Name()

	if _, err = tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("cache write temp: %w", err)
	}
	if err = tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("cache close temp: %w", err)
	}

	dest := filepath.Join(c.dir, name)
	if err = os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("cache rename: %w", err)
	}
	return nil
}

// WithLock acquires an exclusive flock on a file named name inside the cache
// directory, calls fn while holding the lock, then releases it. If the lock
// cannot be acquired within timeout, ErrLockTimeout is returned without calling
// fn. The poll interval is 10 ms.
//
// Linux flock is process-local and NOT re-entrant: the same process calling
// WithLock on the same file twice will self-deadlock. Only one layer of the
// call stack must hold any given lock name at a time.
func (c *Cache) WithLock(name string, timeout time.Duration, fn func() error) error {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return fmt.Errorf("cache mkdir for lock: %w", err)
	}

	lockPath := filepath.Join(c.dir, name)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		return fmt.Errorf("cache open lock file: %w", err)
	}
	defer f.Close()

	deadline := time.Now().Add(timeout)
	const pollInterval = 10 * time.Millisecond

	for {
		lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr == nil {
			break // acquired
		}
		if !errors.Is(lockErr, syscall.EWOULDBLOCK) {
			return fmt.Errorf("cache flock: %w", lockErr)
		}
		if time.Now().After(deadline) {
			return ErrLockTimeout
		}
		time.Sleep(pollInterval)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck

	return fn()
}
