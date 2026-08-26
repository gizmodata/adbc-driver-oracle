// Package ttc implements Oracle's Two-Task Common (TTC) message layer on
// top of package tns: connection handshake and authentication, statement
// execution, row fetching, binds and transaction control.
package ttc

import (
	"errors"
	"fmt"
	"strings"
)

// Error is an error reported by the Oracle Database server (ORA-nnnnn).
type Error struct {
	Code      int
	Message   string
	Offset    int
	RowCount  uint64
	IsWarning bool
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		return fmt.Sprintf("ORA-%05d", e.Code)
	}
	return msg
}

// IsSessionDead reports whether the error implies the connection is gone.
func (e *Error) IsSessionDead() bool {
	switch e.Code {
	case 22, 28, 31, 45, 378, 600, 602, 603, 609, 1012, 1041, 1043, 1089, 1092,
		2396, 3113, 3114, 3122, 3135, 12153, 12537, 12547, 12570, 12583, 27146,
		28511, 56600:
		return true
	}
	return false
}

// AsError extracts an *Error from err, if present.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

var integrityErrorCodes = map[int]bool{
	1: true, 1400: true, 1438: true, 2290: true, 2291: true, 2292: true,
	2296: true, 2297: true, 2299: true, 12899: true,
}
