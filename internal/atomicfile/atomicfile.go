// Package atomicfile writes a file so a crash mid-write never leaves it truncated: content
// goes to a temp file in the SAME directory, is fsync'd, then renamed over the target (an
// atomic operation on POSIX). Load-bearing state files (config.json, plan snapshots, job
// meta, checkpoint manifests) use this so a power loss / SIGKILL can't brick the project.
package atomicfile

import (
	"os"
	"path/filepath"
)

// WriteFile atomically writes data to path with the given permissions. On any failure the
// original file (if any) is left untouched and the temp file is cleaned up.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Best-effort cleanup if we bail before the rename; after a successful rename the temp
	// name no longer exists, so the Remove is a harmless no-op.
	defer func() { _ = os.Remove(tmp) }()

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
