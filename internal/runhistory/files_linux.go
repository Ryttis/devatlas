//go:build linux

package runhistory

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// Every untrusted path component is opened relative to an already open
// directory. No symlink can redirect a read, including during a rename race.
func openDir(parent *os.File, name string) (*os.File, error) {
	flags := syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_DIRECTORY
	var fd int
	var err error
	if parent == nil {
		fd, err = syscall.Open(name, flags, 0)
	} else {
		fd, err = syscall.Openat(int(parent.Fd()), name, flags, 0)
	}
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func readJSON(dir *os.File, name string, limit int64) ([]byte, error) {
	fd, err := syscall.Openat(int(dir.Fd()), name,
		syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 || info.Size() > limit {
		return nil, errors.New("unsupported record file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("record cannot be read")
	}
	return b, nil
}
