//go:build unix

package registryfile

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// lock takes an exclusive advisory lock on lockPath, waiting up to timeout.
// flock locks belong to the open file description, so separate opens in the
// same process contend as well, which keeps the in-process concurrency test
// honest. The lock is released when the returned function is called or when
// the process exits, so a crashed writer never leaves a stale lock.
func lock(lockPath string, timeout time.Duration) (func(), error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", lockPath, err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("lock %s: another devatlas process held it for more than %s", lockPath, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
