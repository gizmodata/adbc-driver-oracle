package oratype

import (
	"strconv"
	"testing"
)

func roundTrip(t *testing.T, s string) {
	t.Helper()
	enc, err := EncodeNumber(nil, []byte(s))
	if err != nil {
		t.Fatalf("encode %q: %v", s, err)
	}
	dec, err := DecodeNumber(enc, nil)
	if err != nil {
		t.Fatalf("decode %q (%x): %v", s, enc, err)
	}
	if string(dec.Text) != s {
		t.Fatalf("round trip %q -> %x -> %q", s, enc, dec.Text)
	}
}

func TestNumberRoundTrip(t *testing.T) {
	for _, s := range []string{
		"0", "1", "-1", "10", "100", "123", "-123", "1234567890", "-1234567890",
		"0.5", "-0.5", "0.05", "123.45", "-123.45", "99999999999999999999",
		"0.000001", "1000000", "12345678901234567890123456789012345678",
		"3.14159265358979", "-9223372036854775808", "9223372036854775807",
	} {
		roundTrip(t, s)
	}
}

func TestKnownEncodings(t *testing.T) {
	// Known wire encodings (from Oracle's DUMP(): 1 -> C1 02, 100 -> C2 02, -1 -> 3E 64 66)
	cases := map[string][]byte{
		"1":    {0xc1, 0x02},
		"100":  {0xc2, 0x02},
		"-1":   {0x3e, 0x64, 0x66},
		"0":    {0x80},
		"0.5":  {0xc0, 0x33},
		"1.5":  {0xc1, 0x02, 0x33},
		"-100": {0x3d, 0x64, 0x66},
	}
	for s, want := range cases {
		got, err := EncodeNumber(nil, []byte(s))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s: got %x want %x", s, got, want)
		}
	}
}

func TestIntegerDecodeInt64(t *testing.T) {
	for _, v := range []int64{0, 1, -1, 42, -42, 1 << 40, -(1 << 40)} {
		enc, _ := EncodeInt64(nil, v)
		dec, err := DecodeNumber(enc, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := strconv.ParseInt(string(dec.Text), 10, 64)
		if err != nil || got != v {
			t.Errorf("%d -> %q (%v)", v, dec.Text, err)
		}
	}
}
