// Package tns implements the Oracle Transparent Network Substrate (TNS)
// packet layer: framing, SDU-aware read/write buffers, the TCP/TLS
// transport and the negotiated capability set. It is the lowest layer of
// the pure-Go Oracle wire protocol implementation; the TTC message layer
// (package ttc) sits on top of it.
//
// The protocol constants and encodings mirror Oracle's own open-source
// python-oracledb "thin" driver, which is the authoritative public
// reference for the wire format.
package tns

// Packet types.
const (
	PacketTypeConnect  uint8 = 1
	PacketTypeAccept   uint8 = 2
	PacketTypeRefuse   uint8 = 4
	PacketTypeRedirect uint8 = 5
	PacketTypeData     uint8 = 6
	PacketTypeResend   uint8 = 11
	PacketTypeMarker   uint8 = 12
	PacketTypeControl  uint8 = 14
)

// Packet flags.
const (
	PacketFlagRedirect uint8 = 0x04
	PacketFlagTLSReneg uint8 = 0x08
)

// Data packet flags.
const (
	DataFlagsBeginPipeline uint16 = 0x1000
	DataFlagsEndOfRequest  uint16 = 0x800
	DataFlagsEndOfResponse uint16 = 0x2000
	DataFlagsEOF           uint16 = 0x0040
)

// Marker types.
const (
	MarkerTypeBreak     uint8 = 1
	MarkerTypeReset     uint8 = 2
	MarkerTypeInterrupt uint8 = 3
)

// Control packet types.
const (
	ControlTypeInbandNotification uint16 = 8
	ControlTypeResetOOB           uint16 = 9
)

// Protocol versions.
const (
	VersionDesired          uint16 = 319
	VersionMinimum          uint16 = 300
	VersionMinAccepted      uint16 = 315 // 12.1
	VersionMinLargeSDU      uint16 = 315
	VersionMinOOBCheck      uint16 = 318
	VersionMinEndOfResponse uint16 = 319
)

// Connect packet flags.
const (
	GSODontCare             uint16 = 0x0001
	GSOCanRecvAttention     uint16 = 0x0400
	NSINARequired           uint8  = 0x10
	NSIDisableNA            uint8  = 0x04
	NSISupportSecurityRen   uint8  = 0x80
	ProtocolCharacteristics uint16 = 0x4f98
	CheckOOB                uint32 = 0x01
	MaxConnectData                 = 230
)

// Accept packet flags.
const (
	AcceptFlagCheckOOB         uint32 = 0x00000001
	AcceptFlagFastAuth         uint32 = 0x10000000
	AcceptFlagHasEndOfResponse uint32 = 0x02000000
)

// Message types (first byte of a TTC message inside a DATA packet).
const (
	MsgTypeProtocol            uint8 = 1
	MsgTypeDataTypes           uint8 = 2
	MsgTypeFunction            uint8 = 3
	MsgTypeError               uint8 = 4
	MsgTypeRowHeader           uint8 = 6
	MsgTypeRowData             uint8 = 7
	MsgTypeParameter           uint8 = 8
	MsgTypeStatus              uint8 = 9
	MsgTypeIOVector            uint8 = 11
	MsgTypeOAC                 uint8 = 13
	MsgTypeLOBData             uint8 = 14
	MsgTypeWarning             uint8 = 15
	MsgTypeDescribeInfo        uint8 = 16
	MsgTypePiggyback           uint8 = 17
	MsgTypeFlushOutBinds       uint8 = 19
	MsgTypeBitVector           uint8 = 21
	MsgTypeServerSidePiggyback uint8 = 23
	MsgTypeOnewayFn            uint8 = 26
	MsgTypeImplicitResultset   uint8 = 27
	MsgTypeRenegotiate         uint8 = 28
	MsgTypeEndOfResponse       uint8 = 29
	MsgTypeToken               uint8 = 33
	MsgTypeFastAuth            uint8 = 34
)

// TNS error numbers surfaced via in-band notifications / data flags.
const (
	ErrInconsistentDataTypes = 932
	ErrVarNotInSelectList    = 1007
	ErrInbandMessage         = 12573
	ErrInvalidServiceName    = 12514
	ErrInvalidSID            = 12505
	ErrNoDataFound           = 1403
	ErrSessionShutdown       = 12572
	ErrArrayDMLErrors        = 24381
	ErrExceededIdleTime      = 2396
)

// Length indicators / limits.
const (
	MaxShortLength      = 252
	LongLengthIndicator = 254
	NullLengthIndicator = 255
	ChunkSize           = 32767
	PacketHeaderSize    = 8
	DefaultSDU          = 8192
	MaxSDU              = 2097152
	EscapeChar          = 253
)

// Character sets.
const (
	CharsetUTF8  uint16 = 873
	CharsetUTF16 uint16 = 2000

	EncodingMultiByte  uint8 = 0x01
	EncodingConvLength uint8 = 0x02
)
