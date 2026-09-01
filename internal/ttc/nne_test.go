package ttc

import (
	"crypto/tls"
	"errors"
	"net"
	"testing"

	"github.com/gizmodata/adbc-driver-oracle/internal/tns"
)

// nneTestConn builds a Conn with a transport but no server, optionally
// with negotiated packet security installed, to exercise the fail-closed
// check in isolation.
func nneTestConn(t *testing.T, cfg *Config, encID, chkID int) *Conn {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	c := &Conn{cfg: cfg, transport: tns.NewTransport(client)}
	if encID != 0 || chkID != 0 {
		sec, err := tns.NewSecurity(encID, chkID, make([]byte, 32), make([]byte, 16))
		if err != nil {
			t.Fatalf("NewSecurity: %v", err)
		}
		c.transport.SetSecurity(sec)
	}
	return c
}

// verifyNNERequirement must fail the connection closed whenever a
// required protection is missing, and pass otherwise.
func TestVerifyNNERequirement(t *testing.T) {
	cases := []struct {
		name         string
		ano          *tns.ANOConfig
		tls          bool
		encID, chkID int
		wantErr      bool
	}{
		{name: "no ANO config", ano: nil},
		{name: "accepted, nothing negotiated",
			ano: &tns.ANOConfig{EncryptionLevel: tns.LevelAccepted, ChecksumLevel: tns.LevelAccepted}},
		{name: "requested, nothing negotiated (best effort)",
			ano: &tns.ANOConfig{EncryptionLevel: tns.LevelRequested, ChecksumLevel: tns.LevelRequested}},
		{name: "required, nothing negotiated",
			ano:     &tns.ANOConfig{EncryptionLevel: tns.LevelRequired, ChecksumLevel: tns.LevelRequired},
			wantErr: true},
		{name: "required, both negotiated",
			ano:   &tns.ANOConfig{EncryptionLevel: tns.LevelRequired, ChecksumLevel: tns.LevelRequired},
			encID: tns.EncryptAES256, chkID: tns.ChecksumSHA256},
		{name: "encryption required, only checksum negotiated",
			ano:     &tns.ANOConfig{EncryptionLevel: tns.LevelRequired, ChecksumLevel: tns.LevelAccepted},
			chkID:   tns.ChecksumSHA256,
			wantErr: true},
		{name: "checksum required, only encryption negotiated",
			ano:     &tns.ANOConfig{EncryptionLevel: tns.LevelAccepted, ChecksumLevel: tns.LevelRequired},
			encID:   tns.EncryptAES256,
			wantErr: true},
		{name: "required over TLS is satisfied",
			ano: &tns.ANOConfig{EncryptionLevel: tns.LevelRequired, ChecksumLevel: tns.LevelRequired},
			tls: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{ANO: tc.ano}
			if tc.tls {
				cfg.TLS = &tls.Config{}
			}
			c := nneTestConn(t, cfg, tc.encID, tc.chkID)
			err := c.verifyNNERequirement()
			if tc.wantErr {
				var oe *Error
				if !errors.As(err, &oe) || oe.Code != 12660 {
					t.Fatalf("expected ORA-12660 fail-closed error, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// NNEInfo must report inactive on an unprotected session and the
// negotiated algorithm names on a protected one.
func TestNNEInfo(t *testing.T) {
	plain := nneTestConn(t, &Config{}, 0, 0)
	if enc, chk, active := plain.NNEInfo(); active || enc != "" || chk != "" {
		t.Fatalf("expected inactive, got %q %q %v", enc, chk, active)
	}
	prot := nneTestConn(t, &Config{}, tns.EncryptAES256, tns.ChecksumSHA512)
	enc, chk, active := prot.NNEInfo()
	if !active || enc != "AES256" || chk != "SHA512" {
		t.Fatalf("expected AES256/SHA512 active, got %q %q %v", enc, chk, active)
	}
}
