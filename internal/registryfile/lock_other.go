//go:build !unix

package registryfile

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// lock on non-Unix systems creates lockPath exclusively and removes it on
// unlock. Unlike flock this lock is not released if the process crashes;
// delete the file by hand in that case. DevAtlas targets WSL, where the
// flock implementation is used; this fallback only keeps other platforms
// compiling and is not verified.
func lock(lockPath string, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create lock file: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("lock %s exists; if no devatlas process is running, delete it", lockPath)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
