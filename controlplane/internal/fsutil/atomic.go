// Package fsutil holds the file handling shared by the stores.
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteAtomic replaces a file's contents in one step, or leaves the old
// contents entirely intact.
//
// Write-in-place would be shorter, but a crash partway through leaves a
// truncated inventory or a half-written lease table, and the daemon comes back
// up having forgotten which subscriber owns which address. The temp file is
// created in the destination directory so the rename cannot cross a filesystem
// boundary, and both the file and its directory are synced so the rename
// survives power loss, not just process death.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("fsutil: create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("fsutil: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()

	// Any failure from here on must not leave the temp file behind.
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("fsutil: write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return fmt.Errorf("fsutil: chmod %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsutil: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("fsutil: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("fsutil: rename %s to %s: %w", tmpName, path, err)
	}

	// Sync the directory so the rename itself is durable. Without this the file
	// can contain the new bytes while the directory entry still points at the
	// old inode after a power cut.
	d, err := os.Open(dir)
	if err != nil {
		return nil // the rename succeeded; durability of the entry is best effort
	}
	defer d.Close()
	_ = d.Sync()
	return nil
}
