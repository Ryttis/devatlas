//go:build !linux

package registryfile

import (
	"errors"
	"os"
	"time"
)

var errUnsupportedPlatform = errors.New("registration persistence is supported only on Linux/WSL")

func lock(string, time.Duration) (func(), error) {
	return nil, errUnsupportedPlatform
}

func regularFile(os.FileInfo) error { return errUnsupportedPlatform }
