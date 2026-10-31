package nodeagent

import (
	"net"

	"golang.org/x/sys/unix"
)

const peerCredSupported = true

func peerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid int
	var cerr error
	err = raw.Control(func(fd uintptr) {
		var cred *unix.Ucred
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if cerr == nil {
			uid = int(cred.Uid)
		}
	})
	if err != nil {
		return 0, err
	}
	return uid, cerr
}
