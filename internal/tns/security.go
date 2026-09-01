package tns

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rc4" //nolint:staticcheck // legacy Oracle keystream for MD5/SHA1 checksums
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"fmt"
	"hash"
)

// Native Network Encryption (Oracle Advanced Networking Option) support:
// after negotiation every DATA packet payload carries a keyed-hash
// trailer and/or is AES-CBC encrypted. The constructions were
// reimplemented in Go by reference to the community go-ora driver, an
// openly-licensed (MIT) implementation.

// Encryption algorithm ids as negotiated with the server.
const (
	EncryptNone   = 0
	EncryptAES128 = 15
	EncryptAES192 = 16
	EncryptAES256 = 17
)

// Data integrity (checksum) algorithm ids.
const (
	ChecksumNone   = 0
	ChecksumMD5    = 1
	ChecksumSHA1   = 3
	ChecksumSHA512 = 4
	ChecksumSHA256 = 5
	ChecksumSHA384 = 6
)

type packetCipher interface {
	encrypt(plain []byte) ([]byte, error)
	decrypt(data []byte) ([]byte, error)
}

// packetHash computes / validates the keyed checksum trailer. Both sides
// keep an evolving keystream, so exactly one compute per outgoing and
// one validate per incoming DATA packet must happen, in order.
type packetHash interface {
	init() error
	compute(payload []byte) []byte
	validate(data []byte) ([]byte, error)
	size() int
}

// Security is the negotiated per-session packet protection.
type Security struct {
	cipher       packetCipher
	hash         packetHash
	EncryptionID int
	ChecksumID   int
}

// Active reports whether any protection is negotiated.
func (s *Security) Active() bool { return s != nil && (s.cipher != nil || s.hash != nil) }

// EncryptionActive reports whether packet payloads are encrypted.
func (s *Security) EncryptionActive() bool { return s != nil && s.cipher != nil }

// ChecksumActive reports whether packet payloads carry a keyed checksum.
func (s *Security) ChecksumActive() bool { return s != nil && s.hash != nil }

// EncryptionAlgorithmName maps a negotiated encryption id to its
// sqlnet.ora-style name ("" for none/unknown).
func EncryptionAlgorithmName(id int) string {
	switch id {
	case EncryptAES128:
		return "AES128"
	case EncryptAES192:
		return "AES192"
	case EncryptAES256:
		return "AES256"
	}
	return ""
}

// ChecksumAlgorithmName maps a negotiated checksum id to its
// sqlnet.ora-style name ("" for none/unknown).
func ChecksumAlgorithmName(id int) string {
	switch id {
	case ChecksumMD5:
		return "MD5"
	case ChecksumSHA1:
		return "SHA1"
	case ChecksumSHA512:
		return "SHA512"
	case ChecksumSHA256:
		return "SHA256"
	case ChecksumSHA384:
		return "SHA384"
	}
	return ""
}

// Overhead is the maximum number of bytes added to a payload.
func (s *Security) Overhead() int {
	if !s.Active() {
		return 0
	}
	n := 1 // folding key byte
	if s.hash != nil {
		n += s.hash.size()
	}
	if s.cipher != nil {
		n += 17 // padding + pad-length byte
	}
	return n
}

// Reset re-initialises the checksum keystream (after a marker reset).
func (s *Security) Reset() error {
	if s != nil && s.hash != nil {
		return s.hash.init()
	}
	return nil
}

func (s *Security) wrap(payload []byte) ([]byte, error) {
	out := payload
	if s.hash != nil {
		out = append(append([]byte(nil), payload...), s.hash.compute(payload)...)
	}
	if s.cipher != nil {
		var err error
		out, err = s.cipher.encrypt(out)
		if err != nil {
			return nil, err
		}
	}
	return append(out, 0), nil // folding key
}

func (s *Security) unwrap(data []byte) ([]byte, error) {
	if len(data) < 1 {
		return nil, errors.New("tns: protected packet too short")
	}
	out := data[:len(data)-1]
	if s.cipher != nil {
		var err error
		out, err = s.cipher.decrypt(out)
		if err != nil {
			return nil, err
		}
	}
	if s.hash != nil {
		var err error
		out, err = s.hash.validate(out)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// NewSecurity builds the protection from the negotiated algorithm ids,
// the Diffie-Hellman shared key and the server-supplied IV.
func NewSecurity(encryptionID, checksumID int, sharedKey, iv []byte) (*Security, error) {
	s := &Security{EncryptionID: encryptionID, ChecksumID: checksumID}
	var err error
	switch checksumID {
	case ChecksumNone:
	case ChecksumMD5:
		s.hash, err = newRC4KeyedHash(md5.New(), sharedKey, iv)
	case ChecksumSHA1:
		s.hash, err = newRC4KeyedHash(sha1.New(), sharedKey, iv)
	case ChecksumSHA512:
		s.hash, err = newAESKeyedHash(sha512.New(), sharedKey, iv)
	case ChecksumSHA256:
		s.hash, err = newAESKeyedHash(sha256.New(), sharedKey, iv)
	case ChecksumSHA384:
		s.hash, err = newAESKeyedHash(sha512.New384(), sharedKey, iv)
	default:
		return nil, fmt.Errorf("tns: checksum algorithm %d is not supported", checksumID)
	}
	if err != nil {
		return nil, err
	}
	switch encryptionID {
	case EncryptNone:
	case EncryptAES128, EncryptAES192, EncryptAES256:
		n := map[int]int{EncryptAES128: 16, EncryptAES192: 24, EncryptAES256: 32}[encryptionID]
		if len(sharedKey) < n {
			return nil, errors.New("tns: shared key too short for the negotiated cipher")
		}
		s.cipher, err = newAESCBCCipher(sharedKey[:n])
	default:
		return nil, fmt.Errorf("tns: encryption algorithm %d is not supported (only AES-128/192/256)", encryptionID)
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

type aesCBCCipher struct{ block cipher.Block }

func newAESCBCCipher(key []byte) (*aesCBCCipher, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return &aesCBCCipher{block: b}, nil
}

func (c *aesCBCCipher) encrypt(plain []byte) ([]byte, error) {
	pad := 0
	if len(plain)%16 != 0 {
		pad = 16 - len(plain)%16
	}
	in := make([]byte, len(plain)+pad)
	copy(in, plain)
	out := make([]byte, len(in)+1)
	cipher.NewCBCEncrypter(c.block, make([]byte, 16)).CryptBlocks(out[:len(in)], in)
	out[len(in)] = byte(pad + 1)
	return out, nil
}

func (c *aesCBCCipher) decrypt(data []byte) ([]byte, error) {
	if len(data) < 1 || (len(data)-1)%16 != 0 {
		return nil, errors.New("tns: invalid encrypted packet length")
	}
	pad := int(data[len(data)-1])
	if pad < 1 || pad > 17 {
		return nil, errors.New("tns: invalid encrypted packet padding")
	}
	body := data[:len(data)-1]
	out := make([]byte, len(body))
	cipher.NewCBCDecrypter(c.block, make([]byte, 16)).CryptBlocks(out, body)
	if pad-1 > len(out) {
		return nil, errors.New("tns: invalid encrypted packet padding")
	}
	return out[:len(out)-(pad-1)], nil
}

// aesKeyedHash: SHA-256/384/512 with an AES-CBC keystream.
type aesKeyedHash struct {
	h         hash.Hash
	buffer    []byte
	outState  []byte
	inState   []byte
	keyGen    cipher.BlockMode
	encryptor cipher.BlockMode
	decryptor cipher.BlockMode
}

func newAESKeyedHash(h hash.Hash, key, iv []byte) (*aesKeyedHash, error) {
	if len(key) < 5 || len(iv) < 16 {
		return nil, errors.New("tns: checksum key material too short")
	}
	k := &aesKeyedHash{h: h, buffer: make([]byte, 32), outState: make([]byte, h.Size()), inState: make([]byte, h.Size())}
	aesKey := make([]byte, 16)
	copy(aesKey[:5], key[:5])
	aesKey[5] = 0xFF
	blk, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	k.keyGen = cipher.NewCBCEncrypter(blk, iv[:16])
	return k, k.init()
}

func (k *aesKeyedHash) init() error {
	k.keyGen.CryptBlocks(k.buffer, k.buffer)
	key := make([]byte, 16)
	copy(key, k.buffer[:16])
	iv := append([]byte(nil), k.buffer[16:32]...)
	blk, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	k.keyGen = cipher.NewCBCEncrypter(blk, iv)
	key[5] = 90
	blk, _ = aes.NewCipher(key)
	k.encryptor = cipher.NewCBCEncrypter(blk, iv)
	key[5] = 180
	blk, _ = aes.NewCipher(key)
	k.decryptor = cipher.NewCBCEncrypter(blk, iv)
	return nil
}

func (k *aesKeyedHash) size() int { return k.h.Size() }

func (k *aesKeyedHash) compute(payload []byte) []byte {
	k.encryptor.CryptBlocks(k.outState, k.outState)
	k.h.Reset()
	k.h.Write(payload)
	k.h.Write(k.outState)
	return k.h.Sum(nil)
}

func (k *aesKeyedHash) validate(data []byte) ([]byte, error) {
	n := k.h.Size()
	if len(data) < n {
		return nil, errors.New("tns: data integrity check failed (short packet)")
	}
	payload, received := data[:len(data)-n], data[len(data)-n:]
	k.decryptor.CryptBlocks(k.inState, k.inState)
	k.h.Reset()
	k.h.Write(payload)
	k.h.Write(k.inState)
	if !bytes.Equal(received, k.h.Sum(nil)) {
		return nil, errors.New("tns: data integrity check failed")
	}
	return payload, nil
}

// rc4KeyedHash: MD5/SHA-1 with an RC4 keystream (legacy scheme).
type rc4KeyedHash struct {
	h         hash.Hash
	keyGen    *rc4.Cipher
	encryptor *rc4.Cipher
	decryptor *rc4.Cipher
}

func newRC4KeyedHash(h hash.Hash, key, iv []byte) (*rc4KeyedHash, error) {
	if len(key) < 5 {
		return nil, errors.New("tns: checksum key material too short")
	}
	k1 := append(append(append([]byte{}, key[len(key)-5:]...), 0xFF), iv...)
	gen, err := rc4.NewCipher(k1)
	if err != nil {
		return nil, err
	}
	k := &rc4KeyedHash{h: h, keyGen: gen}
	return k, k.init()
}

func (k *rc4KeyedHash) init() error {
	key2 := make([]byte, 5)
	k.keyGen.XORKeyStream(key2, make([]byte, 5))
	var err error
	if k.encryptor, err = rc4.NewCipher(append(append([]byte{}, key2...), 90)); err != nil {
		return err
	}
	k.decryptor, err = rc4.NewCipher(append(append([]byte{}, key2...), 180))
	return err
}

func (k *rc4KeyedHash) size() int { return k.h.Size() }

func (k *rc4KeyedHash) compute(payload []byte) []byte {
	ks := make([]byte, k.h.Size())
	k.encryptor.XORKeyStream(ks, ks)
	k.h.Reset()
	k.h.Write(payload)
	k.h.Write(ks)
	return k.h.Sum(nil)
}

func (k *rc4KeyedHash) validate(data []byte) ([]byte, error) {
	n := k.h.Size()
	if len(data) < n {
		return nil, errors.New("tns: data integrity check failed (short packet)")
	}
	payload, received := data[:len(data)-n], data[len(data)-n:]
	ks := make([]byte, n)
	k.decryptor.XORKeyStream(ks, ks)
	k.h.Reset()
	k.h.Write(payload)
	k.h.Write(ks)
	if !bytes.Equal(received, k.h.Sum(nil)) {
		return nil, errors.New("tns: data integrity check failed")
	}
	return payload, nil
}
