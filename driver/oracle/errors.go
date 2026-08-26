package oracle

import (
	"context"
	"errors"
	"net"

	"github.com/apache/arrow-adbc/go/adbc"

	"github.com/gizmodata/adbc-driver-oracle/internal/tns"
	"github.com/gizmodata/adbc-driver-oracle/internal/ttc"
)

// fromTTCError maps wire-layer errors onto ADBC statuses, preserving the
// ORA- error number as the vendor code.
func fromTTCError(err error) error {
	if err == nil {
		return nil
	}
	var ae adbc.Error
	if errors.As(err, &ae) {
		return err
	}
	if oe, ok := ttc.AsError(err); ok {
		return adbc.Error{Code: statusForORA(oe.Code), Msg: oe.Error(), VendorCode: int32(oe.Code)}
	}
	if errors.Is(err, tns.ErrConnectionClosed) {
		return adbc.Error{Code: adbc.StatusIO, Msg: err.Error()}
	}
	if errors.Is(err, context.Canceled) {
		return adbc.Error{Code: adbc.StatusCancelled, Msg: err.Error()}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return adbc.Error{Code: adbc.StatusTimeout, Msg: err.Error()}
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			return adbc.Error{Code: adbc.StatusTimeout, Msg: err.Error()}
		}
		return adbc.Error{Code: adbc.StatusIO, Msg: err.Error()}
	}
	return adbc.Error{Code: adbc.StatusInternal, Msg: err.Error()}
}

func statusForORA(code int) adbc.Status {
	switch code {
	case 942, 4043, 1418, 2289, 1435, 4080, 2443, 12514, 12505:
		return adbc.StatusNotFound
	case 955, 1430, 1408, 2264, 2275, 2260, 2261:
		return adbc.StatusAlreadyExists
	case 1017, 28000, 28001, 1005, 1045:
		return adbc.StatusUnauthenticated
	case 1031, 1950, 2004, 1932:
		return adbc.StatusUnauthorized
	case 1, 1400, 1407, 1438, 2290, 2291, 2292, 2293, 2296, 2297, 2299, 12899:
		return adbc.StatusIntegrity
	case 1013:
		return adbc.StatusCancelled
	case 3113, 3114, 12170, 12541, 12537, 12571, 12570, 12572, 12583, 2396, 1012, 28:
		return adbc.StatusIO
	}
	if code >= 900 && code <= 999 {
		return adbc.StatusInvalidArgument
	}
	if code >= 1700 && code <= 1799 {
		return adbc.StatusInvalidArgument
	}
	return adbc.StatusInternal
}
