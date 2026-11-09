package ttc

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// 10g password hashes (the value SYS.USER$.PASSWORD holds). The default-account
// hashes are published in every default-password list (Pete Finnigan's among
// them), the SYSTEM/SIMON/BOB rows are the test vectors of John the Ripper's
// "oracle" format (src/oracle_fmt_plug.c), and the rows marked "server" were
// read from SYS.USER$ of Oracle XE 21 after CREATE/ALTER USER ... IDENTIFIED BY
// under SQLNET.ALLOWED_LOGON_VERSION_SERVER=8. SCOTT/TIGER, SIMON/THALES2 and
// BOB/LAPIN appear in two of those sources.
var oracle10gVectors = []struct {
	user, password, hash string
}{
	{"SCOTT", "TIGER", "F894844C34402B67"}, // published, server
	{"scott", "tiger", "F894844C34402B67"}, // upper-cased before hashing
	{"SYSTEM", "MANAGER", "D4DF7931AB130E37"},
	{"SYS", "CHANGE_ON_INSTALL", "D4C5016086B2DC6A"},
	{"DBSNMP", "DBSNMP", "E066D214D5421CCC"},
	{"SYSTEM", "THALES", "9EEDFA0AD26C6D52"},
	{"SIMON", "A", "4F8BC1809CB2AF77"},
	{"SIMON", "THALES2", "183D72325548EF11"}, // JtR, server
	{"SIMON", "TST", "C4EB3152E17F24A4"},
	{"BOB", "LAPIN", "B02C8E79ED2E7F46"}, // JtR, server
	{"bob", "GLOUGLOU", "CDC6B483874B875B"},
	{"BOB", "COUCOU_COUCOU", "DA3224126A67C8ED"},
	{"BOB", "LONG_MOT_DE_PASSE_OUI", "EC8147ABB3373D53"},
	{"SCOTT2", "tiger", "C44C11D4C34DB67D"},       // server
	{"SYSTEM2", "manager", "900457D4CE57D1D2"},    // server
	{"MixedCase", "Pa55word", "07B3A79485BCF5B3"}, // server: CREATE USER "MixedCase" IDENTIFIED BY "Pa55word"
}

func TestOracle10gHashVectors(t *testing.T) {
	for _, v := range oracle10gVectors {
		got := strings.ToUpper(hex.EncodeToString(oracle10gHash(v.user, v.password)))
		if got != v.hash {
			t.Errorf("oracle10gHash(%q, %q) = %s, want %s", v.user, v.password, got, v.hash)
		}
	}
}

// aesCBCNoPad encrypts or decrypts whole blocks with a zero IV, as the server does.
func aesCBCNoPad(t *testing.T, key, data []byte, encrypt bool) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(data))
	iv := make([]byte, aes.BlockSize)
	if encrypt {
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, data)
	} else {
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	}
	return out
}

// pbkdf2Block1 is the first PBKDF2-HMAC-SHA512 output block, written out here
// independently of crypto/pbkdf2 so the test does not share the code under test.
func pbkdf2Block1(password, salt []byte, iterations int) []byte {
	mac := hmac.New(sha512.New, password)
	mac.Write(salt)
	mac.Write(binary.BigEndian.AppendUint32(nil, 1))
	u := mac.Sum(nil)
	out := append([]byte{}, u...)
	for i := 1; i < iterations; i++ {
		mac.Reset()
		mac.Write(u)
		u = mac.Sum(nil)
		for j := range out {
			out[j] ^= u[j]
		}
	}
	return out
}

// TestO3LogonSessionKeyAndPassword plays the server's side of an O3LOGON
// exchange: it encrypts a server session key under the account's 10g hash,
// lets the client answer, then recovers the client's session key, derives the
// combined key the way the server does, and decrypts the password.
func TestO3LogonSessionKeyAndPassword(t *testing.T) {
	const user, password = "Scott", "Tiger#1"
	cases := []struct {
		name    string
		cskSalt string // AUTH_PBKDF2_CSK_SALT; empty for servers older than 12c
		sder    int
	}{
		{name: "pbkdf2_combo_key", cskSalt: "8E2F6B9A0C1D2E3F405162738495A6B7", sder: 3},
		{name: "md5_combo_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The server holds the account's 10g hash (checked against published
			// vectors above), zero-extended to an AES-128 key.
			key := append(oracle10gHash(user, password), make([]byte, 8)...)
			serverKey := bytes.Repeat([]byte{0x5a, 0xc3, 0x11, 0x7e}, 8) // 32 bytes
			sd := map[string]string{
				"AUTH_VFR_DATA": "",
				"AUTH_SESSKEY":  strings.ToUpper(hex.EncodeToString(aesCBCNoPad(t, key, serverKey, true))),
			}
			if tc.cskSalt != "" {
				sd["AUTH_PBKDF2_CSK_SALT"] = tc.cskSalt
				sd["AUTH_PBKDF2_SDER_COUNT"] = "3"
			}
			m := &authMessage{username: user, password: []byte(password), verifierType: verifierType10g, sessionData: sd, allow10g: true}
			if err := m.generateVerifier(); err != nil {
				t.Fatal(err)
			}
			if m.speedyKey != "" {
				t.Errorf("10g must not send AUTH_PBKDF2_SPEEDY_KEY, got %q", m.speedyKey)
			}
			if len(m.sessionKey) != 64 || m.sessionKey != strings.ToUpper(m.sessionKey) {
				t.Fatalf("AUTH_SESSKEY should be 64 upper-case hex digits, got %q", m.sessionKey)
			}
			encClient, err := hex.DecodeString(m.sessionKey)
			if err != nil {
				t.Fatal(err)
			}
			clientKey := aesCBCNoPad(t, key, encClient, false)
			if bytes.Equal(clientKey, serverKey) {
				t.Fatal("client session key equals the server's")
			}

			var combo []byte
			if tc.cskSalt != "" {
				salt, _ := hex.DecodeString(tc.cskSalt)
				text := strings.ToUpper(hex.EncodeToString(append(append([]byte{}, clientKey[:16]...), serverKey[:16]...)))
				combo = pbkdf2Block1([]byte(text), salt, tc.sder)[:16]
			} else {
				x := make([]byte, 16)
				for i := range x {
					x[i] = serverKey[16+i] ^ clientKey[16+i]
				}
				sum := md5.Sum(x)
				combo = sum[:]
			}
			if !bytes.Equal(combo, m.comboKey) {
				t.Fatalf("combined key %X, server derives %X", m.comboKey, combo)
			}

			encPassword, err := hex.DecodeString(m.encodedPassword)
			if err != nil {
				t.Fatal(err)
			}
			plain := aesCBCNoPad(t, combo, encPassword, false)
			pad := int(plain[len(plain)-1])
			if pad < 1 || pad > aes.BlockSize {
				t.Fatalf("bad PKCS#7 padding %d", pad)
			}
			if got := string(plain[16 : len(plain)-pad]); got != password {
				t.Fatalf("server decrypts password %q, want %q", got, password)
			}

			// The server's AUTH_SVR_RESPONSE, encrypted under the combined key,
			// must be accepted.
			resp := append(bytes.Repeat([]byte{7}, 16), []byte("SERVER_TO_CLIENT")...)
			m.functionCode = funcAuthPhaseTwo
			m.sessionData["AUTH_SVR_RESPONSE"] = hex.EncodeToString(aesCBCNoPad(t, combo, resp, true))
			if err := m.verifyServerResponse(); err != nil {
				t.Fatal(err)
			}
			m.sessionData["AUTH_SVR_RESPONSE"] = hex.EncodeToString(aesCBCNoPad(t, combo, bytes.Repeat([]byte{9}, 32), true))
			if err := m.verifyServerResponse(); err == nil {
				t.Fatal("a wrong server response was accepted")
			}
		})
	}
}

// Without the opt-in, a 10g challenge is refused before anything derived from
// the password is computed or sent.
func TestO3LogonRefusedWithoutOptIn(t *testing.T) {
	c := &Conn{}
	m := &authMessage{username: "scott", password: []byte("tiger"), verifierType: verifierType10g,
		sessionData: map[string]string{"AUTH_SESSKEY": strings.Repeat("00", 32), "AUTH_VFR_DATA": ""}}
	m.init(c, funcAuthPhaseTwo)
	m.write(nil) // must return before touching the buffer
	if !errors.Is(c.deferredErr, Err10gVerifierNotAllowed) {
		t.Fatalf("deferredErr = %v, want Err10gVerifierNotAllowed", c.deferredErr)
	}
	if m.sessionKey != "" || m.encodedPassword != "" || m.comboKey != nil {
		t.Fatal("key material was derived without the opt-in")
	}
	for _, want := range []string{"adbc.oracle.allow_10g_verifier=true", "10g (DES-based)", "man-in-the-middle"} {
		if !strings.Contains(Err10gVerifierNotAllowed.Error(), want) {
			t.Errorf("error message lacks %q", want)
		}
	}

	// An unknown verifier type is still refused, opt-in or not.
	c2 := &Conn{}
	m2 := &authMessage{username: "scott", password: []byte("tiger"), verifierType: 0x1234, allow10g: true, sessionData: map[string]string{}}
	m2.init(c2, funcAuthPhaseTwo)
	m2.write(nil)
	if c2.deferredErr == nil || !strings.Contains(c2.deferredErr.Error(), "unsupported password verifier type 0x1234") {
		t.Fatalf("deferredErr = %v", c2.deferredErr)
	}
}
