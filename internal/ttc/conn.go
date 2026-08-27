package ttc

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gizmodata/adbc-driver-oracle/internal/tns"
)

// Config describes how to reach and authenticate with the database.
type Config struct {
	Addresses    []Address // tried in order
	ServiceName  string
	SID          string
	InstanceName string
	ServerType   string // "dedicated", "shared", "pooled" or ""
	Username     string
	Password     string
	Token        string
	Mode         uint32 // authModeSysDBA etc. (0 = normal)

	TLS              *tls.Config // nil for plain TCP; non-nil implies tcps
	ConnectTimeout   time.Duration
	SDU              uint32
	Program          string
	Machine          string
	OSUser           string
	Terminal         string
	DriverName       string
	FullVersion      uint32
	SessionTimeZone  string // e.g. "+00:00"; default UTC
	CurrentSchema    string
	ExtraConnectData map[string]string

	// DisableOOB turns off out-of-band break support (statement
	// cancellation then falls back to in-band markers).
	DisableOOB bool

	// ANO configures Native Network Encryption / data integrity. When
	// EncryptionLevel or ChecksumLevel is >= levelAccepted (0) the driver
	// participates in the Advanced Networking negotiation. Nil disables it
	// (the server must then not require NNE).
	ANO *tns.ANOConfig

	// Trace, if set, receives protocol-level debug output.
	Trace func(format string, args ...any)
}

func tnsOOBSupported() bool { return runtime.GOOS != "windows" }

// Address is one listener endpoint.
type Address struct {
	Host     string
	Port     int
	Protocol string // "tcp" or "tcps"
}

// Conn is an authenticated session with an Oracle Database.
type Conn struct {
	mu        sync.Mutex
	cfg       *Config
	transport *tns.Transport
	caps      *tns.Capabilities
	rbuf      *tns.ReadBuffer
	wbuf      *tns.WriteBuffer
	inConnect bool
	closed    bool

	deferredErr error

	txnInProgress   bool
	breakInProgress bool
	breakSent       atomic.Bool

	// session info
	sessionID             uint32
	serialNum             uint16
	serverVersion         [5]int
	serverBanner          string
	dbName                string
	dbDomain              string
	dbUniqueName          string
	serviceName           string
	instanceName          string
	maxOpenCursors        int
	maxIdentifierLen      int
	edition               string
	currentSchema         string
	currentSchemaModified bool
	supportsBool          bool

	cursorsToClose []uint16

	// end-to-end attributes
	action, clientIdentifier, clientInfo, module                                 string
	actionModified, clientIdentifierModified, clientInfoModified, moduleModified bool
	endToEndModified                                                             bool

	connectionID string
	objTypes     *objTypeCache
}

// Dial connects and authenticates.
func Dial(ctx context.Context, cfg *Config) (*Conn, error) {
	if len(cfg.Addresses) == 0 {
		return nil, errors.New("oracle: no host specified")
	}
	if cfg.ServiceName == "" && cfg.SID == "" {
		return nil, errors.New("oracle: a service name or SID is required")
	}
	if cfg.Username == "" && cfg.Token == "" {
		return nil, errors.New("oracle: a username is required")
	}
	fillDefaults(cfg)
	c := &Conn{cfg: cfg, caps: tns.NewCapabilities(), inConnect: true}
	// Out-of-band (TCP urgent) breaks are the only reliable way to cancel
	// a running call; they are available on plain TCP on Unix platforms.
	c.caps.SupportsOOB = cfg.TLS == nil && !cfg.DisableOOB && tnsOOBSupported()
	if cfg.SDU > 0 {
		c.caps.SDU = cfg.SDU
	}
	idBytes := make([]byte, 16)
	_, _ = rand.Read(idBytes)
	c.connectionID = base64.StdEncoding.EncodeToString(idBytes)

	var lastErr error
	for i, addr := range cfg.Addresses {
		err := c.connectAddress(ctx, addr, false)
		if errors.Is(err, errRetryWithANO) {
			// The server requires Native Network Encryption: reconnect to
			// the same address with ANO enabled and negotiate it. Reset the
			// negotiated capabilities so the fresh CONNECT packet is framed
			// as an initial handshake (2-byte length, no protocol version).
			if c.transport != nil {
				_ = c.transport.Close()
				c.transport = nil
			}
			c.caps = tns.NewCapabilities()
			// The NNE pass must not advertise out-of-band breaks: a raw TCP
			// urgent byte bypasses the encrypted packet channel and servers
			// (e.g. Oracle Cloud) reject or hang on it. Cancellation over
			// NNE falls back to in-band interrupt markers.
			c.caps.SupportsOOB = false
			if cfg.SDU > 0 {
				c.caps.SDU = cfg.SDU
			}
			err = c.connectAddress(ctx, addr, true)
		}
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err
		if c.transport != nil {
			_ = c.transport.Close()
			c.transport = nil
		}
		// A refused service/SID is definitive; try the next address only
		// for network-level failures.
		if oe, ok := AsError(err); ok && (oe.Code == tns.ErrInvalidServiceName || oe.Code == tns.ErrInvalidSID) {
			if i == len(cfg.Addresses)-1 {
				return nil, err
			}
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	if err := c.authenticate(ctx); err != nil {
		_ = c.transport.Close()
		return nil, err
	}
	c.inConnect = false
	return c, nil
}

var (
	errRetryWithANO = errors.New("oracle: retry connection with Native Network Encryption")
	errNNERequired  = &Error{Code: 12660, Message: "ORA-12660: the server requires Native Network Encryption / data integrity; enable it with adbc.oracle.nne=accepted (or higher), or connect over TLS (tcps)"}
)

func fillDefaults(cfg *Config) {
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 20 * time.Second
	}
	if cfg.Program == "" {
		if exe, err := os.Executable(); err == nil {
			cfg.Program = filepath.Base(exe)
		} else {
			cfg.Program = "adbc-driver-oracle"
		}
	}
	if cfg.Machine == "" {
		cfg.Machine, _ = os.Hostname()
		if cfg.Machine == "" {
			cfg.Machine = "localhost"
		}
	}
	if cfg.OSUser == "" {
		if u, err := user.Current(); err == nil {
			cfg.OSUser = u.Username
		}
	}
	if cfg.Terminal == "" {
		cfg.Terminal = "unknown"
	}
	if cfg.DriverName == "" {
		cfg.DriverName = driverName
	}
	if cfg.SessionTimeZone == "" {
		cfg.SessionTimeZone = "+00:00"
	}
	for i := range cfg.Addresses {
		if cfg.Addresses[i].Port == 0 {
			cfg.Addresses[i].Port = 1521
		}
		if cfg.Addresses[i].Protocol == "" {
			if cfg.TLS != nil {
				cfg.Addresses[i].Protocol = "tcps"
			} else {
				cfg.Addresses[i].Protocol = "tcp"
			}
		}
	}
}

// connectData builds the (DESCRIPTION=...) string sent to the listener.
func (c *Conn) connectData(addr Address) string {
	cfg := c.cfg
	var sb strings.Builder
	sb.WriteString("(DESCRIPTION=")
	sb.WriteString(fmt.Sprintf("(ADDRESS=(PROTOCOL=%s)(HOST=%s)(PORT=%d))", addr.Protocol, addr.Host, addr.Port))
	sb.WriteString("(CONNECT_DATA=")
	if cfg.ServiceName != "" {
		sb.WriteString("(SERVICE_NAME=" + cfg.ServiceName + ")")
	}
	if cfg.InstanceName != "" {
		sb.WriteString("(INSTANCE_NAME=" + cfg.InstanceName + ")")
	} else if cfg.SID != "" {
		sb.WriteString("(SID=" + cfg.SID + ")")
	}
	if cfg.ServerType != "" {
		sb.WriteString("(SERVER=" + cfg.ServerType + ")")
	}
	sb.WriteString(fmt.Sprintf("(CID=(PROGRAM=%s)(HOST=%s)(USER=%s))", cfg.Program, cfg.Machine, cfg.OSUser))
	for k, v := range cfg.ExtraConnectData {
		sb.WriteString("(" + strings.ToUpper(k) + "=" + v + ")")
	}
	sb.WriteString("(CONNECTION_ID=" + c.connectionID + ")")
	sb.WriteString(")")
	if addr.Protocol == "tcps" {
		sb.WriteString("(SECURITY=(SSL_SERVER_DN_MATCH=ON))")
	}
	sb.WriteString(")")
	return sb.String()
}

func (c *Conn) dialTCP(ctx context.Context, addr Address) error {
	d := net.Dialer{Timeout: c.cfg.ConnectTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(addr.Host, strconv.Itoa(addr.Port)))
	if err != nil {
		return fmt.Errorf("oracle: cannot connect to %s:%d: %w", addr.Host, addr.Port, err)
	}
	c.transport = tns.NewTransport(conn)
	c.transport.Trace = c.cfg.Trace
	c.transport.SetMaxPacketSize(int(c.caps.SDU))
	c.transport.SetReadTimeout(c.cfg.ConnectTimeout)
	c.rbuf = tns.NewReadBuffer(c.transport, c.caps)
	c.wbuf = tns.NewWriteBuffer(c.transport, c.caps)
	if addr.Protocol == "tcps" {
		tcfg := c.cfg.TLS
		if tcfg == nil {
			tcfg = &tls.Config{}
		}
		tcfg = tcfg.Clone()
		if tcfg.ServerName == "" && !tcfg.InsecureSkipVerify {
			tcfg.ServerName = addr.Host
		}
		if err := c.transport.UpgradeTLS(tcfg); err != nil {
			return err
		}
	}
	return nil
}

// connectAddress performs the TNS connect handshake to one address,
// following listener redirects. allowANO enables Native Network
// Encryption negotiation (used on the second pass once the server has
// told us it requires it).
func (c *Conn) connectAddress(ctx context.Context, addr Address, allowANO bool) error {
	if err := c.dialTCP(ctx, addr); err != nil {
		return err
	}
	connectString := c.connectData(addr)
	var packetFlags uint8
	for {
		m := &connectMessage{
			connectString: []byte(connectString),
			packetFlags:   packetFlags,
			sdu:           c.caps.SDU,
			host:          addr.Host,
			port:          addr.Port,
			serviceName:   c.cfg.ServiceName,
			sid:           c.cfg.SID,
			allowANO:      allowANO,
		}
		m.init(c, 0)
		c.rbuf.ResetPackets()
		m.write(c.wbuf)
		if err := c.wbuf.EndRequest(); err != nil {
			return err
		}
		if err := c.receivePacket(&m.baseMessage, false); err != nil {
			return err
		}
		if err := m.processPacket(c.rbuf); err != nil {
			return err
		}
		if m.redirectData != "" {
			pos := strings.IndexByte(m.redirectData, 0)
			if pos < 0 {
				return fmt.Errorf("oracle: invalid redirect data %q", m.redirectData)
			}
			newAddr, err := parseRedirectAddress(m.redirectData[:pos])
			if err != nil {
				return err
			}
			connectString = m.redirectData[pos+1:]
			_ = c.transport.Close()
			newAddr.Protocol = addr.Protocol
			addr = newAddr
			if err := c.dialTCP(ctx, addr); err != nil {
				return err
			}
			packetFlags = tns.PacketFlagRedirect
			continue
		}
		if m.accepted {
			c.transport.SetMaxPacketSize(int(c.caps.SDU))
			c.wbuf.SizeForSDU()
			if m.naRequired && !allowANO {
				// The server requires Native Network Encryption but this
				// pass negotiated with NA disabled. Signal the caller to
				// retry with ANO enabled.
				if c.cfg.ANO == nil {
					return errNNERequired
				}
				return errRetryWithANO
			}
			if allowANO {
				if err := c.negotiateANO(); err != nil {
					return err
				}
			}
			return nil
		}
		if addr.Protocol == "tcps" && c.rbuf.CurrentPacket().Flags&tns.PacketFlagTLSReneg != 0 {
			tcfg := c.cfg.TLS
			if tcfg == nil {
				tcfg = &tls.Config{}
			}
			tcfg = tcfg.Clone()
			if tcfg.ServerName == "" && !tcfg.InsecureSkipVerify {
				tcfg.ServerName = addr.Host
			}
			if err := c.transport.UpgradeTLS(tcfg); err != nil {
				return err
			}
		}
		// RESEND: loop and send the connect packet again.
	}
}

func parseRedirectAddress(desc string) (Address, error) {
	var a Address
	get := func(key string) string {
		idx := strings.Index(strings.ToUpper(desc), "("+key+"=")
		if idx < 0 {
			return ""
		}
		rest := desc[idx+len(key)+2:]
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			return ""
		}
		return strings.TrimSpace(rest[:end])
	}
	a.Host = get("HOST")
	if a.Host == "" {
		return a, fmt.Errorf("oracle: redirect without host: %q", desc)
	}
	a.Port, _ = strconv.Atoi(get("PORT"))
	if a.Port == 0 {
		a.Port = 1521
	}
	a.Protocol = strings.ToLower(get("PROTOCOL"))
	return a, nil
}

// negotiateANO runs the Advanced Networking (NNE) negotiation right after
// ACCEPT and installs packet encryption / checksumming on the transport.
func (c *Conn) negotiateANO(ctx0 ...context.Context) error {
	cfg := c.cfg.ANO
	if cfg == nil {
		return fmt.Errorf("oracle: server requested Native Network Encryption but it is disabled")
	}
	ano := tns.NewANO(c.transport, c.rbuf, c.wbuf, c.caps, *cfg)
	sec, err := ano.Negotiate()
	if err != nil {
		return err
	}
	if sec.Active() {
		c.transport.SetSecurity(sec)
		c.wbuf.SizeForSDU()
		// Out-of-band breaks send a raw TCP urgent byte that bypasses the
		// encrypted/checksummed packet channel; servers reject that once
		// Native Network Encryption is active. Fall back to in-band
		// interrupt markers for cancellation.
		c.caps.SupportsOOB = false
		if c.cfg.Trace != nil {
			c.cfg.Trace("Native Network Encryption active: encryption=%d checksum=%d", sec.EncryptionID, sec.ChecksumID)
		}
	}
	return nil
}

func (c *Conn) authenticate(ctx context.Context) error {
	cfg := c.cfg
	if c.caps.SupportsOOB && !c.transport.OOBSupported() {
		c.caps.SupportsOOB = false
	}
	// If OOB is possible, send an urgent byte followed by a reset marker
	// so the server can tell us (via a control packet) if it does not
	// understand out-of-band data.
	if c.caps.SupportsOOB && c.caps.SupportsOOBCheck {
		if err := c.transport.SendOOB(); err != nil {
			c.caps.SupportsOOB = false
		} else {
			c.sendMarker(tns.MarkerTypeReset)
		}
	}
	protocol := &protocolMessage{}
	protocol.init(c, 0)
	dataTypes := &dataTypesMessage{}
	dataTypes.init(c, 0)
	auth := &authMessage{
		username:       cfg.Username,
		password:       []byte(cfg.Password),
		token:          cfg.Token,
		authMode:       authModeLogon | cfg.Mode,
		program:        cfg.Program,
		terminal:       cfg.Terminal,
		machine:        cfg.Machine,
		osuser:         cfg.OSUser,
		pid:            strconv.Itoa(os.Getpid()),
		driverName:     cfg.DriverName,
		fullVersionNum: cfg.FullVersion,
		alterSession:   fmt.Sprintf("ALTER SESSION SET TIME_ZONE='%s'\x00", strings.ReplaceAll(cfg.SessionTimeZone, "'", "''")),
	}
	auth.initialize(c)

	if c.caps.SupportsFastAuth {
		fast := &fastAuthMessage{protocol: protocol, dataTypes: dataTypes, auth: auth}
		fast.init(c, 0)
		if err := c.processMessage(fast); err != nil {
			return err
		}
	} else {
		supportsEOR := c.caps.SupportsEndOfResponse
		c.caps.SupportsEndOfResponse = false
		if err := c.processMessage(protocol); err != nil {
			return err
		}
		if err := c.processMessage(dataTypes); err != nil {
			return err
		}
		c.caps.SupportsEndOfResponse = supportsEOR
		if err := c.processMessage(auth); err != nil {
			return err
		}
	}
	if auth.resend {
		if err := c.processMessage(auth); err != nil {
			return err
		}
	}
	sd := auth.sessionData
	if v, err := strconv.ParseUint(sd["AUTH_SESSION_ID"], 10, 32); err == nil {
		c.sessionID = uint32(v)
	}
	if v, err := strconv.ParseUint(sd["AUTH_SERIAL_NUM"], 10, 16); err == nil {
		c.serialNum = uint16(v)
	}
	c.dbDomain = sd["AUTH_SC_DB_DOMAIN"]
	c.dbName = sd["AUTH_SC_DBUNIQUE_NAME"]
	c.dbUniqueName = sd["AUTH_SC_REAL_DBUNIQUE_NAME"]
	c.maxOpenCursors, _ = strconv.Atoi(sd["AUTH_MAX_OPEN_CURSORS"])
	c.serviceName = sd["AUTH_SC_SERVICE_NAME"]
	c.instanceName = sd["AUTH_INSTANCENAME"]
	c.maxIdentifierLen, _ = strconv.Atoi(sd["AUTH_MAX_IDEN_LENGTH"])
	if c.maxIdentifierLen == 0 {
		c.maxIdentifierLen = 30
	}
	c.serverVersion = auth.serverVersion(c.caps)
	c.serverBanner = protocol.serverBanner
	c.supportsBool = c.caps.TTCFieldVersion >= tns.FieldVersion23_1
	c.rbuf.ClearPendingError()
	c.transport.SetReadTimeout(0)
	if cfg.CurrentSchema != "" {
		c.currentSchema = cfg.CurrentSchema
		c.currentSchemaModified = true
	}
	return nil
}

// receivePacket waits for the first packet of a response and handles
// markers and refusals.
func (c *Conn) receivePacket(m *baseMessage, checkRequestBoundary bool) error {
	r := c.rbuf
	r.SetCheckRequestBoundary(checkRequestBoundary && c.caps.SupportsEndOfResponse)
	err := r.WaitForPackets(false)
	r.SetCheckRequestBoundary(false)
	if err != nil {
		return err
	}
	switch r.CurrentPacket().Type {
	case tns.PacketTypeMarker:
		if err := c.reset(); err != nil {
			return err
		}
	case tns.PacketTypeRefuse:
		c.wbuf.SetPacketSent(false)
		r.SkipRawBytes(2)
		n := r.ReadUint16BE()
		if n > 0 {
			if cm, ok := any(m).(*baseMessage); ok {
				_ = cm
			}
			b := r.ReadRawBytes(int(n))
			m.errInfo.Message = string(b)
		}
	}
	return r.Err()
}

// reset sends a reset marker and discards packets until the server's
// reset marker arrives; the next packet is then the error response.
func (c *Conn) reset() error {
	c.rbuf.ClearErr()
	c.sendMarker(tns.MarkerTypeReset)
	// Native Network Encryption re-initialises the checksum keystream at
	// the reset boundary (matching the server), so the post-reset packets
	// validate against a fresh keystream. This must happen before any
	// post-reset DATA packet is read (and hence unwrapped) below.
	if sec := c.transport.Security(); sec.Active() {
		if err := sec.Reset(); err != nil {
			return err
		}
	}
	r := c.rbuf
	for {
		p := r.CurrentPacket()
		if p.Type == tns.PacketTypeMarker {
			r.SkipRawBytes(2)
			markerType := r.ReadUB1()
			if markerType == tns.MarkerTypeReset {
				break
			}
		}
		if err := r.WaitForPackets(false); err != nil {
			return err
		}
	}
	for r.CurrentPacket().Type == tns.PacketTypeMarker {
		if err := r.WaitForPackets(false); err != nil {
			return err
		}
	}
	c.breakInProgress = false
	return r.Err()
}

func (c *Conn) sendMarker(markerType uint8) {
	w := c.wbuf
	w.StartRequest(tns.PacketTypeMarker, 0, 0)
	w.WriteUint8(1)
	w.WriteUint8(0)
	w.WriteUint8(markerType)
	_ = w.EndRequest()
}

// processMessage sends a request and processes the complete response.
func (c *Conn) processMessage(m message) error {
	return c.processMessageCtx(context.Background(), m)
}

// sendBreak sends a break marker from another goroutine (statement
// cancellation). The server answers with a reset marker which the
// reading goroutine handles via reset().
func (c *Conn) sendBreak() {
	if c.transport == nil || !c.transport.Connected() || c.inConnect {
		return
	}
	if c.breakSent.Swap(true) {
		return
	}
	if c.caps.SupportsOOB {
		if err := c.transport.SendOOB(); err == nil {
			return
		}
	}
	w := tns.NewWriteBuffer(c.transport, c.caps)
	w.StartRequest(tns.PacketTypeMarker, 0, 0)
	w.WriteUint8(1)
	w.WriteUint8(0)
	w.WriteUint8(tns.MarkerTypeInterrupt)
	_ = w.EndRequest()
}

// Cancel interrupts the call currently in progress on the connection
// (safe to call from another goroutine). The interrupted call returns
// ORA-01013.
func (c *Conn) Cancel() { c.sendBreak() }

// watchContext sends a break when ctx is cancelled while a round trip
// is in progress. The returned func stops the watcher.
func (c *Conn) watchContext(ctx context.Context) func() {
	if ctx == nil || ctx.Done() == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			c.sendBreak()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// processMessageCtx is processMessage with cancellation support: if ctx
// is cancelled mid round trip a break is sent and ctx.Err() is returned.
func (c *Conn) processMessageCtx(ctx context.Context, m message) error {
	if c.transport == nil || !c.transport.Connected() {
		return tns.ErrConnectionClosed
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	stop := c.watchContext(ctx)
	err := c.processMessageLocked(m)
	stop()
	if c.breakSent.Load() {
		// A break went out. Follow python-oracledb: with OOB the urgent
		// byte is followed by an interrupt marker, and the server's
		// marker/error response is consumed so the next request starts
		// clean (it may already have been handled by the reset above).
		if c.transport.Connected() && !errors.Is(err, tns.ErrConnectionClosed) {
			if c.caps.SupportsOOB {
				c.sendMarker(tns.MarkerTypeInterrupt)
			}
			if err == nil {
				if rerr := c.receivePacket(m.base(), false); rerr == nil {
					_ = process(m, c.rbuf)
				}
			}
		}
		c.breakSent.Store(false)
		if ctx != nil && ctx.Err() != nil {
			return fmt.Errorf("%w: %v", ctx.Err(), errOrCancelled(err))
		}
	}
	return err
}

func errOrCancelled(err error) error {
	if err == nil {
		return errors.New("statement cancelled")
	}
	return err
}

func (c *Conn) processMessageLocked(m message) error {
	b := m.base()
	c.deferredErr = nil
	c.rbuf.ResetPackets()
	c.wbuf.StartRequest(tns.PacketTypeData, 0, 0)
	m.write(c.wbuf)
	if c.deferredErr != nil {
		return c.deferredErr
	}
	if err := c.wbuf.EndRequest(); err != nil {
		return err
	}
	if err := c.receivePacket(b, true); err != nil {
		return err
	}
	err := process(m, c.rbuf)
	if errors.Is(err, tns.ErrMarkerDetected) {
		if err = c.reset(); err == nil {
			err = process(m, c.rbuf)
		}
	}
	if err == nil {
		if f, ok := m.(finisher); ok {
			f.finish()
		}
	}
	if err != nil {
		if !c.inConnect && c.wbuf.PacketSent() && c.transport.Connected() && !errors.Is(err, tns.ErrConnectionClosed) {
			c.sendMarker(tns.MarkerTypeBreak)
			_ = c.reset()
		}
		if errors.Is(err, tns.ErrConnectionClosed) {
			c.closed = true
		}
		return err
	}
	if b.flushOutBinds {
		c.wbuf.StartRequest(tns.PacketTypeData, 0, 0)
		c.wbuf.WriteUint8(tns.MsgTypeFlushOutBinds)
		if err := c.wbuf.EndRequest(); err != nil {
			return err
		}
		if err := c.receivePacket(b, false); err != nil {
			return err
		}
		if err := process(m, c.rbuf); err != nil {
			return err
		}
	}
	c.txnInProgress = b.callStatus&eocsFlagsTxnInProgress != 0
	if b.callStatus&eocsFlagsSessRelease != 0 {
		c.cursorsToClose = c.cursorsToClose[:0]
	}
	if b.errorOccurred {
		if b.retry {
			b.errorOccurred = false
			b.retry = false
			return c.processMessageLocked(m)
		}
		e := b.errInfo
		if e.IsSessionDead() {
			c.closed = true
			_ = c.transport.Close()
		}
		return &e
	}
	return nil
}

// finisher is implemented by messages needing post-response processing.
type finisher interface{ finish() }

// Commit commits the current transaction.
func (c *Conn) Commit(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.processMessageCtx(ctx, newSimpleMessage(c, funcCommit))
}

// Rollback rolls back the current transaction.
func (c *Conn) Rollback(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.processMessageCtx(ctx, newSimpleMessage(c, funcRollback))
}

// Ping performs a round trip to the server.
func (c *Conn) Ping(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.processMessageCtx(ctx, newSimpleMessage(c, funcPing))
}

// Close logs off and closes the socket. Uncommitted work is rolled back.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.transport == nil {
		return nil
	}
	c.closed = true
	if c.transport.Connected() {
		if c.txnInProgress {
			_ = c.processMessage(newSimpleMessage(c, funcRollback))
		}
		if c.transport.Connected() {
			_ = c.processMessage(newSimpleMessage(c, funcLogoff))
		}
		if c.transport.Connected() {
			c.wbuf.StartRequest(tns.PacketTypeData, 0, tns.DataFlagsEOF)
			_ = c.wbuf.EndRequest()
		}
	}
	return c.transport.Close()
}

// Closed reports whether the session is unusable.
func (c *Conn) Closed() bool { return c.closed || c.transport == nil || !c.transport.Connected() }

// TxnInProgress reports whether the server says a transaction is open.
func (c *Conn) TxnInProgress() bool { return c.txnInProgress }

// ServerVersion returns the 5-part database version.
func (c *Conn) ServerVersion() [5]int { return c.serverVersion }

// ServerVersionString returns e.g. "23.0.0.0.0".
func (c *Conn) ServerVersionString() string {
	v := c.serverVersion
	return fmt.Sprintf("%d.%d.%d.%d.%d", v[0], v[1], v[2], v[3], v[4])
}

// ServerBanner returns the banner from the protocol negotiation.
func (c *Conn) ServerBanner() string { return c.serverBanner }

// DBName returns the database unique name.
func (c *Conn) DBName() string { return c.dbName }

// ServiceName returns the service name reported at logon.
func (c *Conn) ServiceName() string { return c.serviceName }

// InstanceName returns the instance name.
func (c *Conn) InstanceName() string { return c.instanceName }

// CurrentSchema returns the current schema, if known.
func (c *Conn) CurrentSchema() string { return c.currentSchema }

// SetCurrentSchema changes the current schema on the next round trip.
func (c *Conn) SetCurrentSchema(schema string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.currentSchema = schema
	c.currentSchemaModified = true
}

// SetClientInfo sets end-to-end tracing attributes (module/action/client
// info/identifier) sent with the next round trip.
func (c *Conn) SetClientInfo(module, action, clientInfo, clientIdentifier string, which uint8) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if which&1 != 0 {
		c.module, c.moduleModified = module, true
	}
	if which&2 != 0 {
		c.action, c.actionModified = action, true
	}
	if which&4 != 0 {
		c.clientInfo, c.clientInfoModified = clientInfo, true
	}
	if which&8 != 0 {
		c.clientIdentifier, c.clientIdentifierModified = clientIdentifier, true
	}
	c.endToEndModified = true
}

// SupportsBoolean reports whether the server understands the BOOLEAN
// SQL type (23ai+).
func (c *Conn) SupportsBoolean() bool { return c.supportsBool }

// MaxStringSize returns the negotiated maximum VARCHAR2 size in bytes.
func (c *Conn) MaxStringSize() uint32 { return c.caps.MaxStringSize }

// MaxIdentifierLength returns the maximum identifier length.
func (c *Conn) MaxIdentifierLength() int { return c.maxIdentifierLen }

// SessionID returns the SID/serial of the session.
func (c *Conn) SessionID() (uint32, uint16) { return c.sessionID, c.serialNum }

func (c *Conn) addCursorToClose(id uint16) {
	if id != 0 {
		c.cursorsToClose = append(c.cursorsToClose, id)
	}
}
