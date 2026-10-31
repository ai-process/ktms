//go:build !darwin && !linux

package nodeagent

import (
	"errors"
	"net"
)

// Elsewhere the socket's 0600 mode is the only guard.
const peerCredSupported = false

func peerUID(*net.UnixConn) (int, error) { return 0, errors.New("peer credentials unsupported") }
