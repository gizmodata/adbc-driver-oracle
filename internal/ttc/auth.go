package ttc

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/md5"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/gizmodata/adbc-driver-oracle/internal/tns"
)

// Authentication modes.
const (
	authModeLogon          = 0x00000001
	authModeChangePassword = 0x00000002
	authModeSysDBA         = 0x00000020
	authModeSysOper        = 0x00000040
	authModeWithPassword   = 0x00000100
	authModeSysASM         = 0x00400000
	authModeSysBKP         = 0x01000000
	authModeSysDGD         = 0x02000000
	authModeSysKMT         = 0x04000000
	authModeSysRAC         = 0x08000000
	authModeIAMToken       = 0x20000000
)

// Verifier types.
const (
	verifierType10g  = 0x0939
	verifierType11g1 = 0xb152
	verifierType11g2 = 0x1b25
	verifierType12c  = 0x4815
)

// Err10gVerifierNotAllowed is returned when the server offers only a 10g
// (O3LOGON, DES-based) password verifier for the account and the caller has
// not opted in with Config.Allow10gVerifier. Nothing derived from the
// password has been sent when it is returned.
var Err10gVerifierNotAllowed = errors.New("oracle: the server offered only a 10g (DES-based) password verifier " +
	"for this account, which is refused by default; set adbc.oracle.allow_10g_verifier=true to allow it. " +
	"The 10g hash is weak (DES, case-insensitive, salted only by the user name), and a man-in-the-middle " +
	"on a connection without TLS could request it to get an offline-crackable hash of the password. " +
	"Resetting the account's password on a current server gives it a 12c verifier instead")

// authMessage performs the two-phase O5LOGON authentication.
type authMessage struct {
	baseMessage
	username       string
	password       []byte
	token          string
	authMode       uint32
	program        string
	terminal       string
	machine        string
	osuser         string
	pid            string
	driverName     string
	connectString  string
	alterSession   string
	fullVersionNum uint32
	allow10g       bool

	sessionData     map[string]string
	verifierType    uint32
	sessionKey      string
	speedyKey       string
	encodedPassword string
	comboKey        []byte
}

func (m *authMessage) initialize(c *Conn) {
	m.init(c, funcAuthPhaseOne)
	m.sessionData = map[string]string{}
	m.resend = true
	if m.token != "" {
		m.functionCode = funcAuthPhaseTwo
		m.resend = false
	}
}

func (m *authMessage) processMessage(r *tns.ReadBuffer, msgType uint8) error {
	return m.baseMessage.processMessage(r, msgType)
}

func (m *authMessage) processReturnParameters(r *tns.ReadBuffer) error {
	numParams := r.ReadUB2()
	for i := 0; i < int(numParams); i++ {
		key := r.ReadStrWithLength()
		value := r.ReadStrWithLength()
		if key == "AUTH_VFR_DATA" {
			m.verifierType = r.ReadUB4()
		} else {
			r.SkipUB4()
		}
		m.sessionData[key] = value
	}
	if r.Err() != nil {
		return r.Err()
	}
	if m.functionCode == funcAuthPhaseOne {
		m.functionCode = funcAuthPhaseTwo
	} else if m.comboKey != nil {
		return m.verifyServerResponse()
	}
	return nil
}

// verifyServerResponse checks AUTH_SVR_RESPONSE, which proves the server knows
// the password verifier: it must decrypt under the combined session key.
func (m *authMessage) verifyServerResponse() error {
	encoded, ok := m.sessionData["AUTH_SVR_RESPONSE"]
	if !ok {
		return fmt.Errorf("oracle: server did not return an authentication response")
	}
	raw, err := hex.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("oracle: invalid server authentication response: %w", err)
	}
	resp, err := decryptCBC(m.comboKey, raw)
	if err != nil || len(resp) < 32 || string(resp[16:32]) != "SERVER_TO_CLIENT" {
		return fmt.Errorf("oracle: invalid server authentication response (server key mismatch)")
	}
	return nil
}

func (m *authMessage) writeKeyValue(w *tns.WriteBuffer, key, value string, flags uint32) {
	w.WriteBytesWithTwoLengths([]byte(key))
	w.WriteBytesWithTwoLengths([]byte(value))
	w.WriteUB4(flags)
}

func (m *authMessage) write(w *tns.WriteBuffer) {
	userBytes := []byte(m.username)
	var hasUser uint8
	if len(userBytes) > 0 {
		hasUser = 1
	}
	var numPairs uint32
	if m.functionCode == funcAuthPhaseOne {
		numPairs = 5
	} else {
		numPairs = 4
		if m.token != "" {
			numPairs++
		} else {
			numPairs += 2
			m.authMode |= authModeWithPassword
			switch m.verifierType {
			case verifierType12c:
				numPairs++
			case verifierType11g1, verifierType11g2:
			case verifierType10g:
				if !m.allow10g {
					m.conn.deferredErr = Err10gVerifierNotAllowed
					return
				}
			default:
				m.conn.deferredErr = fmt.Errorf("oracle: unsupported password verifier type %#x", m.verifierType)
				return
			}
			if err := m.generateVerifier(); err != nil {
				m.conn.deferredErr = err
				return
			}
		}
		if m.connectString != "" {
			numPairs++
		}
	}
	m.writeFunctionCode(w)
	w.WriteUint8(hasUser)
	w.WriteUB4(uint32(len(userBytes)))
	w.WriteUB4(m.authMode)
	w.WriteUint8(1) // pointer (authivl)
	w.WriteUB4(numPairs)
	w.WriteUint8(1) // pointer (authovl)
	w.WriteUint8(1) // pointer (authovln)
	if hasUser == 1 {
		w.WriteBytesWithLength(userBytes)
	}
	if m.functionCode == funcAuthPhaseOne {
		m.writeKeyValue(w, "AUTH_TERMINAL", m.terminal, 0)
		m.writeKeyValue(w, "AUTH_PROGRAM_NM", m.program, 0)
		m.writeKeyValue(w, "AUTH_MACHINE", m.machine, 0)
		m.writeKeyValue(w, "AUTH_PID", m.pid, 0)
		m.writeKeyValue(w, "AUTH_SID", m.osuser, 0)
		return
	}
	if m.token != "" {
		m.writeKeyValue(w, "AUTH_TOKEN", m.token, 0)
	} else {
		m.writeKeyValue(w, "AUTH_SESSKEY", m.sessionKey, 1)
		if m.verifierType == verifierType12c {
			m.writeKeyValue(w, "AUTH_PBKDF2_SPEEDY_KEY", m.speedyKey, 0)
		}
		m.writeKeyValue(w, "AUTH_PASSWORD", m.encodedPassword, 0)
	}
	m.writeKeyValue(w, "SESSION_CLIENT_CHARSET", "873", 0)
	m.writeKeyValue(w, "SESSION_CLIENT_DRIVER_NAME", m.driverName, 0)
	m.writeKeyValue(w, "SESSION_CLIENT_VERSION", strconv.FormatUint(uint64(m.fullVersionNum), 10), 0)
	m.writeKeyValue(w, "AUTH_ALTER_SESSION", m.alterSession, 1)
	if m.connectString != "" {
		m.writeKeyValue(w, "AUTH_CONNECT_STRING", m.connectString, 0)
	}
}

func (m *authMessage) generateVerifier() error {
	verifierData, err := hex.DecodeString(m.sessionData["AUTH_VFR_DATA"])
	if err != nil {
		return fmt.Errorf("oracle: invalid AUTH_VFR_DATA: %w", err)
	}
	var keyLen int
	var passwordHash, passwordKey []byte
	switch m.verifierType {
	case verifierType12c:
		keyLen = 32
		iterations, _ := strconv.Atoi(m.sessionData["AUTH_PBKDF2_VGEN_COUNT"])
		salt := append(append([]byte{}, verifierData...), []byte("AUTH_PBKDF2_SPEEDY_KEY")...)
		passwordKey, err = pbkdf2.Key(sha512.New, string(m.password), salt, iterations, 64)
		if err != nil {
			return err
		}
		h := sha512.New()
		h.Write(passwordKey)
		h.Write(verifierData)
		passwordHash = h.Sum(nil)[:32]
	case verifierType10g:
		// O3LOGON: the stored 10g hash, zero-extended to an AES-128 key.
		keyLen = 16
		passwordHash = append(oracle10gHash(m.username, string(m.password)), make([]byte, 8)...)
	default:
		keyLen = 24
		h := sha1.New()
		h.Write(m.password)
		h.Write(verifierData)
		passwordHash = append(h.Sum(nil), 0, 0, 0, 0)
	}
	encodedServerKey, err := hex.DecodeString(m.sessionData["AUTH_SESSKEY"])
	if err != nil {
		return fmt.Errorf("oracle: invalid AUTH_SESSKEY: %w", err)
	}
	partA, err := decryptCBC(passwordHash, encodedServerKey)
	if err != nil {
		return err
	}
	partB := make([]byte, len(partA))
	if _, err := rand.Read(partB); err != nil {
		return err
	}
	encodedClientKey, err := encryptCBC(passwordHash, partB, false)
	if err != nil {
		return err
	}
	var comboKey []byte
	if m.verifierType == verifierType10g {
		m.sessionKey = strings.ToUpper(hex.EncodeToString(encodedClientKey))[:2*len(partB)]
		comboKey, err = o3logonComboKey(partA, partB, m.sessionData)
		if err != nil {
			return err
		}
	} else if len(partA) == 48 {
		m.sessionKey = strings.ToUpper(hex.EncodeToString(encodedClientKey))[:96]
		b := make([]byte, 24)
		for i := 16; i < 40; i++ {
			b[i-16] = partA[i] ^ partB[i]
		}
		p1 := md5.Sum(b[:16])
		p2 := md5.Sum(b[16:])
		comboKey = append(append([]byte{}, p1[:]...), p2[:]...)[:keyLen]
	} else {
		m.sessionKey = strings.ToUpper(hex.EncodeToString(encodedClientKey))[:64]
		salt, err := hex.DecodeString(m.sessionData["AUTH_PBKDF2_CSK_SALT"])
		if err != nil {
			return fmt.Errorf("oracle: invalid AUTH_PBKDF2_CSK_SALT: %w", err)
		}
		iterations, _ := strconv.Atoi(m.sessionData["AUTH_PBKDF2_SDER_COUNT"])
		temp := append(append([]byte{}, partB[:keyLen]...), partA[:keyLen]...)
		comboKey, err = pbkdf2.Key(sha512.New, strings.ToUpper(hex.EncodeToString(temp)), salt, iterations, keyLen)
		if err != nil {
			return err
		}
	}
	m.comboKey = comboKey
	if m.verifierType == verifierType12c {
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return err
		}
		speedy, err := encryptCBC(comboKey, append(salt, passwordKey...), false)
		if err != nil {
			return err
		}
		m.speedyKey = strings.ToUpper(hex.EncodeToString(speedy[:80]))
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	encPassword, err := encryptCBC(comboKey, append(salt, m.password...), false)
	if err != nil {
		return err
	}
	m.encodedPassword = strings.ToUpper(hex.EncodeToString(encPassword))
	return nil
}

// oracle10gHash returns the 8-byte 10g password verifier of username and
// password, the value SYS.USER$.PASSWORD holds (for example SCOTT/TIGER gives
// F894844C34402B67): the user name and password are concatenated, upper-cased
// and encoded as UTF-16BE, zero-padded to the DES block size, and
// DES-CBC-encrypted (zero IV) under the fixed key 0123456789ABCDEF; the last
// cipher block is then the key for a second DES-CBC pass over the same input,
// whose last block is the hash.
//
// Ported from sijms/go-ora (MIT License, see NOTICE), getKeyFromUserNameAndPassword
// in v2/auth_object.go, with UTF-16BE encoding of non-ASCII characters (go-ora
// keeps only the low byte of each UTF-8 byte; for ASCII both agree).
func oracle10gHash(username, password string) []byte {
	units := utf16.Encode([]rune(strings.ToUpper(username + password)))
	n := len(units) * 2
	if n%des.BlockSize != 0 {
		n += des.BlockSize - n%des.BlockSize
	}
	buf := make([]byte, n)
	for i, u := range units {
		binary.BigEndian.PutUint16(buf[2*i:], u)
	}
	desCBCMAC := func(key []byte) []byte {
		block, _ := des.NewCipher(key) // DES keys are always 8 bytes here
		out := make([]byte, n)
		cipher.NewCBCEncrypter(block, make([]byte, des.BlockSize)).CryptBlocks(out, buf)
		return out[n-des.BlockSize:]
	}
	return desCBCMAC(desCBCMAC([]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}))
}

// o3logonComboKey derives the 16-byte AES key that encrypts the password from
// the decrypted server session key (serverKey) and the client's own session
// key (clientKey), both 32 bytes. Servers that send AUTH_PBKDF2_CSK_SALT (12c
// and later) use PBKDF2-HMAC-SHA512 over the hex text of the first halves of
// the client and server keys; older servers use MD5 of the XOR of bytes 16-31.
//
// Ported from sijms/go-ora (MIT License, see NOTICE), generatePasswordEncKey in
// v2/auth_object.go (verifier type 2361).
func o3logonComboKey(serverKey, clientKey []byte, sessionData map[string]string) ([]byte, error) {
	if len(serverKey) != 32 || len(clientKey) != 32 {
		return nil, fmt.Errorf("oracle: unexpected 10g session key length %d", len(serverKey))
	}
	if saltHex, ok := sessionData["AUTH_PBKDF2_CSK_SALT"]; ok {
		salt, err := hex.DecodeString(saltHex)
		if err != nil {
			return nil, fmt.Errorf("oracle: invalid AUTH_PBKDF2_CSK_SALT: %w", err)
		}
		iterations, _ := strconv.Atoi(sessionData["AUTH_PBKDF2_SDER_COUNT"])
		temp := append(append([]byte{}, clientKey[:16]...), serverKey[:16]...)
		return pbkdf2.Key(sha512.New, strings.ToUpper(hex.EncodeToString(temp)), salt, iterations, 16)
	}
	x := make([]byte, 16)
	for i := range x {
		x[i] = serverKey[16+i] ^ clientKey[16+i]
	}
	sum := md5.Sum(x)
	return sum[:], nil
}

// serverVersion returns the 5-tuple server version from AUTH_VERSION_NO.
func (m *authMessage) serverVersion(caps *tns.Capabilities) [5]int {
	v, _ := strconv.ParseUint(m.sessionData["AUTH_VERSION_NO"], 10, 32)
	full := uint32(v)
	if caps.TTCFieldVersion >= tns.FieldVersion18_1Ext1 {
		return [5]int{int(full >> 24 & 0xff), int(full >> 16 & 0xff), int(full >> 12 & 0x0f), int(full >> 4 & 0xff), int(full & 0x0f)}
	}
	return [5]int{int(full >> 24 & 0xff), int(full >> 20 & 0x0f), int(full >> 12 & 0x0f), int(full >> 8 & 0x0f), int(full & 0x0f)}
}

func decryptCBC(key, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("oracle: encrypted data is not a multiple of the AES block size")
	}
	iv := make([]byte, aes.BlockSize)
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	return out, nil
}

func encryptCBC(key, plain []byte, zeros bool) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	n := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, len(plain)+n)
	copy(padded, plain)
	if !zeros {
		for i := len(plain); i < len(padded); i++ {
			padded[i] = byte(n)
		}
	}
	iv := make([]byte, aes.BlockSize)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out, nil
}
