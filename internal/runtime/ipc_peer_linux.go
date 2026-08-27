//go:build linux

package runtime

import (
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func verifyLocalPeer(connection net.Conn) error {
	value, ok := connection.(syscall.Conn)
	if !ok {
		return ErrPeerUnauthorized
	}
	raw, err := value.SyscallConn()
	if err != nil {
		return ErrPeerUnauthorized
	}
	var (
		uid     uint32
		peerErr error
	)
	if err := raw.Control(func(fd uintptr) {
		credential, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			peerErr = err
			return
		}
		uid = credential.Uid
	}); err != nil || peerErr != nil || uid != uint32(os.Geteuid()) {
		return ErrPeerUnauthorized
	}
	return nil
}
