package privcheck

import "testing"

func SetStandardDirsForTest(t *testing.T, dirs []string) {
	old := standardDirs
	standardDirs = dirs
	t.Cleanup(func() { standardDirs = old })
}
