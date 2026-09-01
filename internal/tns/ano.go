package tns

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Advanced Networking Option (ANO) negotiation: exchanged as a series of
// DATA packets immediately after the protocol handshake's data-types
// message would run — actually right after ACCEPT, before the TTC
// protocol message. This negotiates encryption and data-integrity
// (checksum) services, performing a Diffie-Hellman key exchange when a
// checksum or encryption service is selected.
//
// Reimplemented in Go by reference to the community go-ora driver, an
// openly-licensed (MIT) implementation.

const anoVersion = 0xB200200
const anoMagic = 0xDEADBEEF

// Service levels.
const (
	LevelAccepted  = 0
	LevelRejected  = 1
	LevelRequested = 2
	LevelRequired  = 3

	levelAccepted  = LevelAccepted
	levelRejected  = LevelRejected
	levelRequested = LevelRequested
	levelRequired  = LevelRequired
)

// ParseANOLevel maps a sqlnet.ora-style level to its code (-1 unknown).
func ParseANOLevel(s string) int {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "", "ACCEPTED":
		return levelAccepted
	case "REJECTED":
		return levelRejected
	case "REQUESTED":
		return levelRequested
	case "REQUIRED":
		return levelRequired
	}
	return -1
}

// ANOConfig requests specific client-side encryption / checksum levels.
type ANOConfig struct {
	EncryptionLevel int      // level* code
	ChecksumLevel   int      // level* code
	Encryption      []string // preferred algorithms; nil = all supported
	Checksum        []string
}

// anoWriter/anoReader accumulate the ANO sub-packet stream inside one TTC
// write/read buffer.
type anoBuf struct {
	w *WriteBuffer
	r *ReadBuffer
}

// service is one negotiated ANO service (encryption / integrity / auth /
// supervisor).
type anoService struct {
	serviceType int
	level       int
	names       []string
	ids         []int
	selected    []int // indices into names/ids
}

func (s *anoService) buildList(pref []string, useLevel bool) error {
	s.selected = s.selected[:0]
	if useLevel {
		if s.level == levelRejected {
			s.selected = append(s.selected, 0)
			return nil
		}
		if s.level != levelAccepted && s.level != levelRequested && s.level != levelRequired {
			return fmt.Errorf("tns: unsupported ANO service level %d", s.level)
		}
	}
	if len(pref) == 0 {
		for i := 1; i < len(s.names); i++ {
			s.selected = append(s.selected, i)
		}
	} else {
		if useLevel && s.level == levelAccepted {
			s.selected = append(s.selected, 0)
		}
		for _, item := range pref {
			found := false
			for i := 1; i < len(s.names); i++ {
				if strings.EqualFold(strings.TrimSpace(item), s.names[i]) {
					s.selected = append(s.selected, i)
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("tns: unsupported ANO algorithm %q", item)
			}
		}
	}
	if useLevel && s.level == levelRequested {
		s.selected = append(s.selected, 0)
	}
	return nil
}

// ANO holds the negotiation state.
type ANO struct {
	transport *Transport
	rbuf      *ReadBuffer
	wbuf      *WriteBuffer
	caps      *Capabilities

	encrypt  *anoService
	checksum *anoService
	auth     *anoService
	supArray []int

	encAlgoID int
	intAlgoID int
	publicKey []byte
	sharedKey []byte
	iv        []byte
}

// NewANO builds the negotiator for the given transport.
func NewANO(t *Transport, r *ReadBuffer, w *WriteBuffer, caps *Capabilities, cfg ANOConfig) *ANO {
	encLevel := cfg.EncryptionLevel
	intLevel := cfg.ChecksumLevel
	a := &ANO{
		transport: t, rbuf: r, wbuf: w, caps: caps,
		supArray: []int{4, 1, 2, 3},
		encrypt: &anoService{serviceType: 2, level: encLevel,
			names: []string{"", "RC4_40", "RC4_56", "RC4_128", "RC4_256", "DES40C", "DES56C", "3DES112", "3DES168", "AES128", "AES192", "AES256"},
			ids:   []int{0, 1, 8, 10, 6, 3, 2, 11, 12, 15, 16, 17}},
		checksum: &anoService{serviceType: 3, level: intLevel,
			names: []string{"", "MD5", "SHA1", "SHA512", "SHA256", "SHA384"},
			ids:   []int{0, 1, 3, 4, 5, 6}},
		auth: &anoService{serviceType: 1, level: -1,
			names: []string{"", "NTS", "KERBEROS5", "TCPS"}, ids: []int{0, 1, 1, 2}},
	}
	// Only offer AES ciphers — RC4/DES are refused rather than used.
	encPref := cfg.Encryption
	if encPref == nil {
		encPref = []string{"AES128", "AES192", "AES256"}
	}
	_ = a.encrypt.buildList(encPref, true)
	_ = a.checksum.buildList(cfg.Checksum, true)
	_ = a.auth.buildList(nil, false)
	return a
}

// ShouldNegotiate reports whether the server's ACCEPT flags asked for ANO.
// flags1 is the first NSI flag byte from the ACCEPT packet.
func ShouldNegotiate(flags1 uint8) bool {
	return flags1&NSINARequired != 0
}

// Negotiate runs the full exchange and, if a service was selected,
// returns the packet Security to install on the transport.
func (a *ANO) Negotiate() (*Security, error) {
	if err := a.write(); err != nil {
		return nil, err
	}
	if err := a.read(); err != nil {
		return nil, err
	}
	if a.intAlgoID == 0 && a.encAlgoID == 0 {
		return nil, nil
	}
	return NewSecurity(a.encAlgoID, a.intAlgoID, a.sharedKey, a.iv)
}

// ---- sub-packet helpers (each is a 4-byte header + body) ----

func (a *ANO) putHeader(length, ptype int) {
	a.wbuf.WriteUint16BE(uint16(length))
	a.wbuf.WriteUint16BE(uint16(ptype))
}

func (a *ANO) putUB1(v uint8)     { a.putHeader(1, 2); a.wbuf.WriteUint8(v) }
func (a *ANO) putUB2(v int)       { a.putHeader(2, 3); a.wbuf.WriteUint16BE(uint16(v)) }
func (a *ANO) putUB4(v int)       { a.putHeader(4, 4); a.wbuf.WriteUint32BE(uint32(v)) }
func (a *ANO) putVersion(v int)   { a.putHeader(4, 5); a.wbuf.WriteUint32BE(uint32(v)) }
func (a *ANO) putStatus(v int)    { a.putHeader(2, 6); a.wbuf.WriteUint16BE(uint16(v)) }
func (a *ANO) putBytes(b []byte)  { a.putHeader(len(b), 1); a.wbuf.WriteBytes(b) }
func (a *ANO) putString(s string) { a.putHeader(len(s), 0); a.wbuf.WriteStr(s) }

func (a *ANO) serviceHeader(serviceType, sub int) {
	a.wbuf.WriteUint16BE(uint16(serviceType))
	a.wbuf.WriteUint16BE(uint16(sub))
	a.wbuf.WriteUint32BE(0)
}

func (a *ANO) write() error {
	a.wbuf.StartRequest(PacketTypeData, 0, 0)
	// header
	size := 0
	size += 12 + len(a.supArray)*2 + 4 + 12 // supervisor (approx; server ignores exact)
	// We compute lengths exactly below; the header length is informational.
	a.putANOHeader(a.totalLength(), 4, 0)
	a.writeSupervisor()
	a.writeAuth()
	a.writeEncrypt()
	a.writeChecksum()
	return a.wbuf.EndRequest()
}

func (a *ANO) putANOHeader(length, servCount int, errFlags uint8) {
	a.wbuf.WriteUint32BE(anoMagic)
	a.wbuf.WriteUint16BE(uint16(length))
	a.wbuf.WriteUint32BE(anoVersion)
	a.wbuf.WriteUint16BE(uint16(servCount))
	a.wbuf.WriteUint8(errFlags)
}

func (a *ANO) totalLength() int {
	l := 13
	l += 8 + a.supLen()
	l += 8 + a.authLen()
	l += 8 + a.encLen()
	l += 8 + a.intLen()
	return l
}

func (a *ANO) supLen() int { return 12 + 8 + 4 + 10 + len(a.supArray)*2 }
func (a *ANO) authLen() int {
	size := 20
	for _, idx := range a.auth.selected {
		size += 5 + 4 + len(a.auth.names[idx])
	}
	return size
}
func (a *ANO) encLen() int { return 17 + len(a.encrypt.selected) }
func (a *ANO) intLen() int { return 12 + len(a.checksum.selected) }

func (a *ANO) writeSupervisor() {
	a.serviceHeader(4, 3)
	a.putVersion(anoVersion)
	a.putBytes([]byte{0, 0, 16, 28, 102, 236, 40, 234}) // cid
	// ub2 array
	a.putHeader(10+len(a.supArray)*2, 1)
	a.wbuf.WriteUint32BE(anoMagic)
	a.wbuf.WriteUint16BE(3)
	a.wbuf.WriteUint32BE(uint32(len(a.supArray)))
	for _, v := range a.supArray {
		a.wbuf.WriteUint16BE(uint16(v))
	}
}

func (a *ANO) writeAuth() {
	a.serviceHeader(1, 3+len(a.auth.selected)*2)
	a.putVersion(anoVersion)
	a.putUB2(0xE0E1)
	a.putStatus(0xFCFF)
	for _, idx := range a.auth.selected {
		a.putUB1(uint8(a.auth.ids[idx]))
		a.putString(a.auth.names[idx])
	}
}

func (a *ANO) writeEncrypt() {
	a.serviceHeader(2, 3)
	a.putVersion(anoVersion)
	sel := make([]byte, len(a.encrypt.selected))
	for i, idx := range a.encrypt.selected {
		sel[i] = uint8(a.encrypt.ids[idx])
	}
	a.putBytes(sel)
	a.putUB1(1) // driver flag
}

func (a *ANO) writeChecksum() {
	a.serviceHeader(3, 2)
	a.putVersion(anoVersion)
	sel := make([]byte, len(a.checksum.selected))
	for i, idx := range a.checksum.selected {
		sel[i] = uint8(a.checksum.ids[idx])
	}
	a.putBytes(sel)
}

// ---- reading the server response ----

func (a *ANO) read() error {
	if err := a.rbuf.WaitForPackets(false); err != nil {
		return err
	}
	numServices, err := a.readANOHeader()
	if err != nil {
		return err
	}
	for i := 0; i < numServices; i++ {
		sType, subPackets, errCode, err := a.readServiceHeader()
		if err != nil {
			return err
		}
		if errCode != 0 {
			// 12660 in the encryption (2) or checksum (3) service means the
			// levels are incompatible — e.g. the client requires Native
			// Network Encryption and the server rejects it.
			if errCode == 12660 && (sType == 2 || sType == 3) {
				return errors.New("tns: ORA-12660: the required Native Network Encryption / data integrity level is incompatible with the server (the server rejects it); connect over TLS, or lower adbc.oracle.nne")
			}
			return fmt.Errorf("tns: ANO negotiation error ORA-%d in service %d", errCode, sType)
		}
		switch sType {
		case 1:
			err = a.readAuth(subPackets)
		case 2:
			err = a.readEncrypt()
		case 3:
			err = a.readChecksum(subPackets)
		case 4:
			err = a.readSupervisor()
		default:
			return fmt.Errorf("tns: unexpected ANO service %d", sType)
		}
		if err != nil {
			return err
		}
	}
	// If the server sent DH parameters, respond with the client public key.
	if len(a.publicKey) > 0 {
		a.wbuf.StartRequest(PacketTypeData, 0, 0)
		a.putANOHeader(12+len(a.publicKey)+13, 1, 0)
		a.serviceHeader(3, 1)
		a.putBytes(a.publicKey)
		if err := a.wbuf.EndRequest(); err != nil {
			return err
		}
	}
	return a.rbuf.Err()
}

func (a *ANO) readANOHeader() (int, error) {
	magic := a.rbuf.ReadUint32BE()
	if magic != anoMagic {
		return 0, errors.New("tns: bad ANO response header")
	}
	a.rbuf.ReadUint16BE() // length
	a.rbuf.ReadUint32BE() // version
	n := a.rbuf.ReadUint16BE()
	a.rbuf.ReadUB1() // error flags
	return int(n), a.rbuf.Err()
}

func (a *ANO) readServiceHeader() (sType, subPackets, errCode int, err error) {
	sType = int(a.rbuf.ReadUint16BE())
	subPackets = int(a.rbuf.ReadUint16BE())
	errCode = int(a.rbuf.ReadUint32BE())
	return sType, subPackets, errCode, a.rbuf.Err()
}

func (a *ANO) readPacketHeader(want int) (int, error) {
	length := int(a.rbuf.ReadUint16BE())
	ptype := int(a.rbuf.ReadUint16BE())
	if ptype != want {
		return 0, fmt.Errorf("tns: ANO sub-packet type %d, expected %d", ptype, want)
	}
	return length, a.rbuf.Err()
}

func (a *ANO) readVersion() (int, error) {
	if _, err := a.readPacketHeader(5); err != nil {
		return 0, err
	}
	return int(a.rbuf.ReadUint32BE()), a.rbuf.Err()
}
func (a *ANO) readUB1() (uint8, error) {
	if _, err := a.readPacketHeader(2); err != nil {
		return 0, err
	}
	return a.rbuf.ReadUB1(), a.rbuf.Err()
}
func (a *ANO) readUB2() (int, error) {
	if _, err := a.readPacketHeader(3); err != nil {
		return 0, err
	}
	return int(a.rbuf.ReadUint16BE()), a.rbuf.Err()
}
func (a *ANO) readUB4() (int, error) {
	if _, err := a.readPacketHeader(4); err != nil {
		return 0, err
	}
	return int(a.rbuf.ReadUint32BE()), a.rbuf.Err()
}
func (a *ANO) readStatus() (int, error) {
	if _, err := a.readPacketHeader(6); err != nil {
		return 0, err
	}
	return int(a.rbuf.ReadUint16BE()), a.rbuf.Err()
}
func (a *ANO) readBytes() ([]byte, error) {
	length, err := a.readPacketHeader(1)
	if err != nil {
		return nil, err
	}
	b := a.rbuf.ReadRawBytes(length)
	return append([]byte(nil), b...), a.rbuf.Err()
}
func (a *ANO) readString() (string, error) {
	length, err := a.readPacketHeader(0)
	if err != nil {
		return "", err
	}
	b := a.rbuf.ReadRawBytes(length)
	return string(b), a.rbuf.Err()
}

func (a *ANO) readSupervisor() error {
	if _, err := a.readVersion(); err != nil {
		return err
	}
	status, err := a.readStatus()
	if err != nil {
		return err
	}
	if status != 31 {
		return errors.New("tns: bad ANO supervisor status")
	}
	// ub2 array
	if _, err := a.readPacketHeader(1); err != nil {
		return err
	}
	if a.rbuf.ReadUint32BE() != anoMagic {
		return errors.New("tns: bad ANO supervisor array")
	}
	a.rbuf.ReadUint16BE()
	n := a.rbuf.ReadUint32BE()
	for i := 0; i < int(n); i++ {
		a.rbuf.ReadUint16BE()
	}
	return a.rbuf.Err()
}

func (a *ANO) readAuth(subPackets int) error {
	if _, err := a.readVersion(); err != nil {
		return err
	}
	status, err := a.readStatus()
	if err != nil {
		return err
	}
	if status == 0xFAFF && subPackets > 2 {
		if _, err := a.readUB1(); err != nil {
			return err
		}
		name, err := a.readString()
		if err != nil {
			return err
		}
		if subPackets > 4 {
			a.readVersion()
			a.readUB4()
			a.readUB4()
		}
		if name == "KERBEROS5" || name == "NTS" {
			return fmt.Errorf("tns: %s network authentication is not supported", name)
		}
	} else if status != 0xFBFF {
		return errors.New("tns: bad ANO authentication status")
	}
	return a.rbuf.Err()
}

func (a *ANO) readEncrypt() error {
	if _, err := a.readVersion(); err != nil {
		return err
	}
	id, err := a.readUB1()
	if err != nil {
		return err
	}
	a.encAlgoID = int(id)
	return nil
}

func (a *ANO) readChecksum(subPackets int) error {
	if _, err := a.readVersion(); err != nil {
		return err
	}
	id, err := a.readUB1()
	if err != nil {
		return err
	}
	a.intAlgoID = int(id)
	if subPackets != 8 {
		return nil
	}
	dhGenLen, err := a.readUB2()
	if err != nil {
		return err
	}
	dhPrimeLen, err := a.readUB2()
	if err != nil {
		return err
	}
	genBytes, err := a.readBytes()
	if err != nil {
		return err
	}
	primeBytes, err := a.readBytes()
	if err != nil {
		return err
	}
	serverPub, err := a.readBytes()
	if err != nil {
		return err
	}
	a.iv, err = a.readBytes()
	if err != nil {
		return err
	}
	if dhGenLen <= 0 || dhPrimeLen <= 0 {
		return errors.New("tns: bad Diffie-Hellman parameters from server")
	}
	byteLen := (dhGenLen + 7) / 8
	if len(serverPub) != byteLen || len(primeBytes) != byteLen {
		return errors.New("tns: Diffie-Hellman negotiation out of sync")
	}
	priv := make([]byte, byteLen)
	if _, err := rand.Read(priv); err != nil {
		return err
	}
	gen := new(big.Int).SetBytes(genBytes)
	prime := new(big.Int).SetBytes(primeBytes)
	privInt := new(big.Int).SetBytes(priv)
	serverPubInt := new(big.Int).SetBytes(serverPub)
	pub := new(big.Int).Exp(gen, privInt, prime)
	shared := new(big.Int).Exp(serverPubInt, privInt, prime)
	a.publicKey = make([]byte, byteLen)
	pub.FillBytes(a.publicKey)
	a.sharedKey = make([]byte, byteLen)
	shared.FillBytes(a.sharedKey)
	return a.rbuf.Err()
}

var _ = binary.BigEndian
