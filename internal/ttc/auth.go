package ttc

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

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
	verifierType11g1 = 0xb152
	verifierType11g2 = 0x1b25
	verifierType12c  = 0x4815
)

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
	if m.verifierType == verifierType12c {
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
	} else {
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
	if len(partA) == 48 {
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
