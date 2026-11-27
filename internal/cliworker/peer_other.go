//go:build !linux

package cliworker

import (
	"errors"
	"net"
)

func peerUID(net.Conn) (int, error) { return 0, errors.New("peer credentials need Linux") }
