//go:build !windows

package tns

import (
	"errors"
	"net"
	"syscall"
)

// sendOOB writes one byte of TCP urgent (out-of-band) data, which Oracle
// uses as the attention signal for cancelling an in-progress call.
func sendOOB(conn net.Conn) error {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return errors.New("tns: out-of-band data requires a plain TCP connection")
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	err = raw.Write(func(fd uintptr) bool {
		serr = syscall.Sendto(int(fd), []byte{'!'}, syscall.MSG_OOB, nil)
		return true
	})
	if err != nil {
		return err
	}
	return serr
}

// oobSupported reports whether this platform can send urgent data.
func oobSupported() bool { return true }
