//go:build windows

package tns

import (
	"errors"
	"net"
)

func sendOOB(conn net.Conn) error {
	return errors.New("tns: out-of-band breaks are not supported on Windows")
}

func oobSupported() bool { return false }
