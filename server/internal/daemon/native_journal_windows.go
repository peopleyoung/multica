//go:build windows

package daemon

// File.Sync persists the result before rename; directory handles do not support
// the Unix directory-fsync operation on Windows.
func syncNativeJournalDir(string) error { return nil }
