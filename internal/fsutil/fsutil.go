// Package fsutil holds small file-system helpers.
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic replaces path with data: it writes a new temp file in the
// same directory (so a reader never sees a half-written file), fsyncs it,
// sets perm and renames it over path. On any error the temp file is removed.
// The directory itself is not synced: a caller that needs the rename to
// survive a power loss syncs it.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	// filepath.Dir, not Split: a bare filename gives "." (the same directory),
	// where Split's "" would make CreateTemp use $TMPDIR and break the rename.
	dir, base := filepath.Dir(path), filepath.Base(path)
	// CreateTemp makes the file 0600 with a fresh name, so neither a temp
	// file left by a crash nor the umask can widen its mode.
	f, err := os.CreateTemp(dir, "."+base+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()      // may already be closed; the error is being returned
			_ = os.Remove(tmp) // best-effort cleanup
		}
	}()
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", tmp, err)
	}
	if err = f.Chmod(perm); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename to %s: %w", path, err)
	}
	return nil
}
