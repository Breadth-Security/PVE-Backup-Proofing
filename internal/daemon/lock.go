package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrBusy means another run holds the run lock.
var ErrBusy = errors.New("a run is already in progress")

// AcquireLock takes an exclusive, non-blocking flock on path, creating it
// and its directory if needed. The kernel drops the lock if the process
// dies. Every caller of run.Run or run.Preflight must hold it: the janitor
// destroys owned guests that are not in its own state, so two overlapping
// runs would destroy each other's guests.
func AcquireLock(path string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("run lock: create dir: %w", err)
	}
	// #nosec G304 -- path comes from LockPath(), PBP's fixed lock file location, not user input
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("run lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("run lock %s: %w", path, err)
	}
	return func() {
		// Closing the descriptor drops the lock even if the explicit
		// unlock fails, so neither error is actionable.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
