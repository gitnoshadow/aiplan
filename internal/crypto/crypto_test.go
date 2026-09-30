package crypto

import (
	"bytes"
	"testing"
)

func TestRoundTripAndTamper(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	ct, err := Encrypt(key, []byte("refresh-token"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := Decrypt(key, ct)
	if err != nil || string(pt) != "refresh-token" {
		t.Fatalf("round trip failed: %v", err)
	}
	ct[len(ct)-1] ^= 1
	if _, err := Decrypt(key, ct); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
	if _, err := Encrypt([]byte("short"), []byte("x")); err == nil {
		t.Fatal("bad key must fail")
	}
}
