//go:build !linux

package cliworker

import (
	"errors"
	"net"
)

// peerUID is needed only with isolation, which is Linux's.
func peerUID(net.Conn) (int, error) { return 0, errors.New("peer credentials need Linux") }
