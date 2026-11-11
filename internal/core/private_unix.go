//go:build !windows

package core

import (
	"fmt"
	"os"
	"syscall"
)

// CheckPrivateDir refuses a state directory that someone else owns or can write
// to: audit records hold before-states and must not be planted or read by others.
func CheckPrivateDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("state directory %s is owned by another user", dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("state directory %s is writable by others (mode %04o): run chmod 700 on it", dir, fi.Mode().Perm())
	}
	return nil
}
