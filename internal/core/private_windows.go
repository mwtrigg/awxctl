//go:build windows

package core

// CheckPrivateDir has nothing to check on Windows: the profile directory is
// private to the user by its ACL.
func CheckPrivateDir(string) error { return nil }
