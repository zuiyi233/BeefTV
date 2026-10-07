//go:build !windows

package privatefile

import (
	"fmt"
	"os"
)

func verify(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		return fmt.Errorf("%w: %s 权限为 %o，期望 600", ErrNotPrivate, path, perm)
	}
	return nil
}
