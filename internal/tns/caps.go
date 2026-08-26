package tns

// Compile-time capability indices.
const (
	CCapSQLVersion           = 0
	CCapLogonTypes           = 4
	CCapFeatureBackport      = 5
	CCapFieldVersion         = 7
	CCapServerDefineConv     = 8
	CCapDequeueWithSelector  = 9
	CCapTTC1                 = 15
	CCapOCI1                 = 16
	CCapTDSVersion           = 17
	CCapRPCVersion           = 18
	CCapRPCSig               = 19
	CCapDBFVersion           = 21
	CCapLOB                  = 23
	CCapTTC2                 = 26
	CCapUB2DTY               = 27
	CCapOCI2                 = 31
	CCapClientFn             = 34
	CCapOCI3                 = 35
	CCapTTC3                 = 37
	CCapSessSignatureVersion = 39
	CCapTTC4                 = 40
	CCapLOB2                 = 42
	CCapTTC5                 = 44
	CCapFeatureBackport2     = 45
	CCapVectorFeatures       = 52
	CCapTTC6                 = 54
	CCapMax                  = 55
)

// Compile-time capability values.
const (
	CCapSQLVersionMax = 6

	FieldVersion11_2     = 6
	FieldVersion12_1     = 7
	FieldVersion12_2     = 8
	FieldVersion12_2Ext1 = 9
	FieldVersion18_1     = 10
	FieldVersion18_1Ext1 = 11
	FieldVersion19_1     = 12
	FieldVersion19_1Ext1 = 13
	FieldVersion20_1     = 14
	FieldVersion20_1Ext1 = 15
	FieldVersion21_1     = 16
	FieldVersion23_1     = 17
	FieldVersion23_1Ext1 = 18
	FieldVersion23_1Ext2 = 19
	FieldVersion23_1Ext3 = 20
	FieldVersion23_1Ext4 = 21
	FieldVersion23_1Ext5 = 22
	FieldVersion23_3Ext6 = 23
	FieldVersion23_4     = 24
	FieldVersionMax      = 24

	CCapO5Logon                = 8
	CCapO5LogonNP              = 2
	CCapO7Logon                = 32
	CCapO8LogonLongIdentifier  = 64
	CCapO9LogonLongPassword    = 0x80
	CCapCTBImplicitPool        = 0x08
	CCapCTBOAuthMsgOnErr       = 0x10
	CCapEndOfCallStatus        = 0x01
	CCapIndRCD                 = 0x08
	CCapFastBVec               = 0x20
	CCapFastSessionPropagate   = 0x10
	CCapAppCtxPiggyback        = 0x80
	CCapTDSVersionMax          = 3
	CCapRPCVersionMax          = 7
	CCapRPCSigValue            = 3
	CCapDBFVersionMax          = 1
	CCapLTXID                  = 0x08
	CCapImplicitResults        = 0x10
	CCapBigChunkCLR            = 0x20
	CCapKeepOutOrder           = 0x80
	CCapLOBUB8Size             = 0x01
	CCapLOBEncs                = 0x02
	CCapLOBPrefetchData        = 0x04
	CCapLOBTempSize            = 0x08
	CCapLOBPrefetchLength      = 0x40
	CCapLOB12C                 = 0x80
	CCapLOB2Quasi              = 0x01
	CCapLOB22GBPrefetch        = 0x04
	CCapDRCP                   = 0x10
	CCapZLNP                   = 0x04
	CCapInbandNotification     = 0x04
	CCapExplicitBoundary       = 0x40
	CCapEndOfResponse          = 0x20
	CCapClientFnMax            = 12
	CCapVectorSupport          = 0x08
	CCapTokenSupported         = 0x02
	CCapPipeliningSupport      = 0x04
	CCapPipeliningBreak        = 0x10
	CCapVectorFeatureBinary    = 0x01
	CCapVectorFeatureSparse    = 0x02
	CCapTTC5SessionlessTxns    = 0x20
	CCapOCI3OCSSync            = 0x20
	CCapTTC6HAReadiness        = 0x04
	CCapEndUserSecCtxPiggyback = 0x02
)

// Runtime capability indices and values.
const (
	RCapCompat = 0
	RCapTTC    = 6
	RCapMax    = 11

	RCapCompat81           = 2
	RCapTTCZeroCopy        = 0x01
	RCapTTC32K             = 0x04
	RCapTTCSessionStateOps = 0x10
)

// Capabilities holds the negotiated protocol capabilities for a session.
type Capabilities struct {
	ProtocolVersion           uint16
	TTCFieldVersion           uint8
	CharsetID                 uint16
	NCharsetID                uint16
	CompileCaps               []byte
	RuntimeCaps               []byte
	MaxStringSize             uint32
	SupportsFastAuth          bool
	SupportsOOB               bool
	SupportsOOBCheck          bool
	SupportsEndOfResponse     bool
	SupportsPipelining        bool
	SupportsRequestBoundaries bool
	SupportsHAReadiness       bool
	SDU                       uint32
}

// NewCapabilities returns the client's initial capability set.
func NewCapabilities() *Capabilities {
	c := &Capabilities{SDU: DefaultSDU, TTCFieldVersion: FieldVersionMax}
	c.initCompileCaps()
	c.initRuntimeCaps()
	return c
}

func (c *Capabilities) initCompileCaps() {
	cc := make([]byte, CCapMax)
	cc[CCapSQLVersion] = CCapSQLVersionMax
	cc[CCapLogonTypes] = CCapO5Logon | CCapO5LogonNP | CCapO7Logon |
		CCapO8LogonLongIdentifier | CCapO9LogonLongPassword
	cc[CCapFeatureBackport] = CCapCTBImplicitPool | CCapCTBOAuthMsgOnErr
	cc[CCapFieldVersion] = c.TTCFieldVersion
	cc[CCapServerDefineConv] = 1
	cc[CCapDequeueWithSelector] = 1
	cc[CCapTTC1] = CCapFastBVec | CCapEndOfCallStatus | CCapIndRCD
	cc[CCapOCI1] = CCapFastSessionPropagate | CCapAppCtxPiggyback
	cc[CCapTDSVersion] = CCapTDSVersionMax
	cc[CCapRPCVersion] = CCapRPCVersionMax
	cc[CCapRPCSig] = CCapRPCSigValue
	cc[CCapDBFVersion] = CCapDBFVersionMax
	cc[CCapLOB] = CCapLOBUB8Size | CCapLOBEncs | CCapLOBPrefetchLength |
		CCapLOBTempSize | CCapLOB12C | CCapLOBPrefetchData
	cc[CCapUB2DTY] = 1
	cc[CCapLOB2] = CCapLOB2Quasi | CCapLOB22GBPrefetch
	cc[CCapTTC3] = CCapImplicitResults | CCapBigChunkCLR | CCapKeepOutOrder | CCapLTXID
	cc[CCapTTC2] = CCapZLNP
	cc[CCapOCI2] = CCapDRCP
	cc[CCapClientFn] = CCapClientFnMax
	cc[CCapSessSignatureVersion] = FieldVersion12_2
	cc[CCapTTC4] = CCapInbandNotification | CCapExplicitBoundary
	cc[CCapTTC5] = CCapVectorSupport | CCapTokenSupported | CCapPipeliningSupport |
		CCapPipeliningBreak | CCapTTC5SessionlessTxns
	cc[CCapVectorFeatures] = CCapVectorFeatureBinary | CCapVectorFeatureSparse
	cc[CCapOCI3] = CCapOCI3OCSSync
	cc[CCapTTC6] = CCapTTC6HAReadiness
	cc[CCapFeatureBackport2] = CCapEndUserSecCtxPiggyback
	c.CompileCaps = cc
}

func (c *Capabilities) initRuntimeCaps() {
	rc := make([]byte, RCapMax)
	rc[RCapCompat] = RCapCompat81
	rc[RCapTTC] = RCapTTCZeroCopy | RCapTTC32K
	c.RuntimeCaps = rc
}

// AdjustForProtocol applies the server's ACCEPT packet values.
func (c *Capabilities) AdjustForProtocol(protocolVersion, protocolOptions uint16, flags uint32) {
	c.ProtocolVersion = protocolVersion
	c.SupportsOOB = protocolOptions&GSOCanRecvAttention != 0
	if flags&AcceptFlagFastAuth != 0 {
		c.SupportsFastAuth = true
	}
	if flags&AcceptFlagCheckOOB != 0 {
		c.SupportsOOBCheck = true
	}
	if protocolVersion >= VersionMinEndOfResponse && flags&AcceptFlagHasEndOfResponse != 0 {
		c.CompileCaps[CCapTTC4] |= CCapEndOfResponse
		c.SupportsEndOfResponse = true
		c.SupportsPipelining = true
	}
}

// AdjustForServerCompileCaps merges the server's compile-time caps.
func (c *Capabilities) AdjustForServerCompileCaps(server []byte) {
	if len(server) > CCapFieldVersion && server[CCapFieldVersion] < c.TTCFieldVersion {
		c.TTCFieldVersion = server[CCapFieldVersion]
		c.CompileCaps[CCapFieldVersion] = c.TTCFieldVersion
	}
	if len(server) > CCapTTC4 && server[CCapTTC4]&CCapExplicitBoundary != 0 {
		c.SupportsRequestBoundaries = true
	}
	if len(server) > CCapTTC6 && server[CCapTTC6]&CCapTTC6HAReadiness != 0 {
		c.SupportsHAReadiness = true
	}
}

// AdjustForServerRuntimeCaps merges the server's runtime caps.
func (c *Capabilities) AdjustForServerRuntimeCaps(server []byte) {
	if len(server) > RCapTTC && server[RCapTTC]&RCapTTC32K != 0 {
		c.MaxStringSize = 32767
	} else {
		c.MaxStringSize = 4000
	}
	if len(server) > RCapTTC && server[RCapTTC]&RCapTTCSessionStateOps == 0 {
		c.SupportsRequestBoundaries = false
	}
}
