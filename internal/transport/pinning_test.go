package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"math/big"
	"testing"
	"time"
)

func mustGenCert(t *testing.T) (*x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return cert, der
}

func TestPinning_SkipMode_Accepts(t *testing.T) {
	_, der := mustGenCert(t)
	v := NewPinValidator("api.surfbot.io", true, false)
	if err := v.VerifyPeerCertificate([][]byte{der}, nil); err != nil {
		t.Fatalf("skip mode should accept: %v", err)
	}
}

func TestPinning_PlaceholderAllow_Accepts(t *testing.T) {
	_, der := mustGenCert(t)
	// hostPins still has PLACEHOLDER, allowMissing=true → fail-open until rotation.
	v := NewPinValidator("api.surfbot.io", false, true)
	if err := v.VerifyPeerCertificate([][]byte{der}, nil); err != nil {
		t.Fatalf("placeholder allow should accept: %v", err)
	}
}

func TestPinning_RejectsBadCert(t *testing.T) {
	cert, der := mustGenCert(t)
	// Fabricate a "real" pin list that does not match our generated cert.
	host := "test.pinning.example"
	hostPins[host] = []string{"sha256/AAAA1111BBBB2222CCCC3333DDDD4444EEEE5555FFFF0000abcd="}
	t.Cleanup(func() { delete(hostPins, host) })

	v := NewPinValidator(host, false, false)
	err := v.VerifyPeerCertificate([][]byte{der}, nil)
	if err == nil {
		t.Fatalf("expected pin mismatch error")
	}
	if !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("err = %v, want wraps ErrPinMismatch", err)
	}
	_ = cert
}

func TestPinning_AcceptsMatchingPin(t *testing.T) {
	cert, der := mustGenCert(t)
	host := "test.pinning.example"
	got := "sha256/" + base64.StdEncoding.EncodeToString(spkiSHA256(cert))
	hostPins[host] = []string{got}
	t.Cleanup(func() { delete(hostPins, host) })

	v := NewPinValidator(host, false, false)
	if err := v.VerifyPeerCertificate([][]byte{der}, nil); err != nil {
		t.Fatalf("matching pin should accept: %v", err)
	}
}

func TestPinning_UnknownHost_FailsClosed(t *testing.T) {
	_, der := mustGenCert(t)
	v := NewPinValidator("not.in.pin.list", false, false)
	if err := v.VerifyPeerCertificate([][]byte{der}, nil); err == nil {
		t.Fatalf("unknown host with strict mode should reject")
	}
}
