//go:build linux

package cliworker

// ParseStatForTest exposes the parsing of /proc/<pid>/stat to the package's external tests.
var ParseStatForTest = parseStat
