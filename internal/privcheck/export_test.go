package privcheck

import "testing"

// SetStandardDirsForTest replaces the standard directories for t.
func SetStandardDirsForTest(t *testing.T, dirs []string) {
	old := standardDirs
	standardDirs = dirs
	t.Cleanup(func() { standardDirs = old })
}
