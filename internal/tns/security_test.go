package tns

import (
	"bytes"
	"testing"
)

func TestAESCBCPacketRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	c, err := newAESCBCCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 1, 15, 16, 17, 100, 8192} {
		in := make([]byte, n)
		for i := range in {
			in[i] = byte(i * 7)
		}
		enc, err := c.encrypt(in)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := c.decrypt(enc)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !bytes.Equal(in, dec) {
			t.Fatalf("n=%d round trip mismatch", n)
		}
	}
}

// mirrorHash swaps the encrypt/decrypt keystreams so a second Security
// models the *other* end of the wire (the server labels the two RC4/AES
// sub-keys 90/180 the opposite way from the client).
func mirrorHash(s *Security) {
	switch h := s.hash.(type) {
	case *aesKeyedHash:
		h.encryptor, h.decryptor = h.decryptor, h.encryptor
	case *rc4KeyedHash:
		h.encryptor, h.decryptor = h.decryptor, h.encryptor
	}
}

func TestKeyedHashComputeValidate(t *testing.T) {
	key := make([]byte, 24)
	iv := make([]byte, 16)
	for i := range key {
		key[i] = byte(i + 1)
	}
	for i := range iv {
		iv[i] = byte(i * 3)
	}
	// Two independent hash instances driven in lockstep (like the two ends
	// of a connection) must agree, because the keystream evolves per call.
	for id := range []int{ChecksumMD5, ChecksumSHA1, ChecksumSHA256, ChecksumSHA512, ChecksumSHA384} {
		cid := []int{ChecksumMD5, ChecksumSHA1, ChecksumSHA256, ChecksumSHA512, ChecksumSHA384}[id]
		sender, err := NewSecurity(EncryptNone, cid, key, iv)
		if err != nil {
			t.Fatal(err)
		}
		receiver, err := NewSecurity(EncryptNone, cid, key, iv)
		if err != nil {
			t.Fatal(err)
		}
		mirrorHash(receiver)
		for round := 0; round < 4; round++ {
			payload := []byte("packet payload number ")
			payload = append(payload, byte('0'+round))
			wrapped, err := sender.wrap(payload)
			if err != nil {
				t.Fatal(err)
			}
			got, err := receiver.unwrap(wrapped)
			if err != nil {
				t.Fatalf("checksum %d round %d: %v", cid, round, err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("checksum %d round %d payload mismatch", cid, round)
			}
		}
	}
}

func TestEncryptAndChecksumTogether(t *testing.T) {
	key := make([]byte, 32)
	iv := make([]byte, 16)
	for i := range key {
		key[i] = byte(255 - i)
	}
	for i := range iv {
		iv[i] = byte(i)
	}
	a, _ := NewSecurity(EncryptAES256, ChecksumSHA256, key, iv)
	b, _ := NewSecurity(EncryptAES256, ChecksumSHA256, key, iv)
	mirrorHash(b)
	if !a.Active() || a.Overhead() == 0 {
		t.Fatal("expected active security with overhead")
	}
	for round := 0; round < 3; round++ {
		payload := bytes.Repeat([]byte{byte(round)}, 300)
		w, err := a.wrap(payload)
		if err != nil {
			t.Fatal(err)
		}
		got, err := b.unwrap(w)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatal("mismatch")
		}
	}
	// A tampered packet must fail the integrity check.
	payload := []byte("hello")
	w, _ := a.wrap(payload)
	w[2] ^= 0xff
	if _, err := b.unwrap(w); err == nil {
		t.Fatal("expected integrity failure on tampered packet")
	}
}
