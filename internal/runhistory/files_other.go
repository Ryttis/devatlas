//go:build !linux

package runhistory

import (
	"errors"
	"os"
)

func openDir(_ *os.File, _ string) (*os.File, error) {
	return nil, errors.New("run history requires Linux")
}
func readJSON(_ *os.File, _ string, _ int64) ([]byte, error) {
	return nil, errors.New("run history requires Linux")
}
