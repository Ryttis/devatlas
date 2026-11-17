// Package atomicfile writes small files so that readers see either the old
// or the new complete content, never a partial file.
package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Write stores data at path with mode 0600 by writing a temporary file in
// the same directory, syncing it, and renaming it over the target.
func Write(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	fail := func(step string, err error) error {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("%s: %w", step, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail("set permissions on temporary file", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail("write temporary file", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("sync temporary file", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace file: %w", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("file replaced, but open directory for sync failed: %w", err)
	}
	if err := errors.Join(d.Sync(), d.Close()); err != nil {
		return fmt.Errorf("file replaced, but directory sync failed: %w", err)
	}
	return nil
}
