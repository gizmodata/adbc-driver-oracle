// Package oracle implements an Apache Arrow ADBC driver for Oracle
// Database over the TNS/TTC wire protocol, in pure Go — no Oracle
// Client / Instant Client libraries required.
package oracle

// DriverName / VendorName are reported via GetInfo.
const (
	DriverName = "ADBC Oracle Driver - Go"
	VendorName = "Oracle Database"
)
