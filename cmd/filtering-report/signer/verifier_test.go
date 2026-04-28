// Copyright 2026, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

package signer

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/offchainlabs/nitro/cmd/filtering-report/signer/signertest"
)

// newTestSigner builds a Signer whose leaf was issued by a fresh PKI.
// Returns the Signer and the PKI so callers can use the same CA for the verifier
// or use it as the "other" CA when negative-testing chain validation.
func newTestSigner(t *testing.T, leafOpts signertest.LeafOptions) (*Signer, *signertest.PKI) {
	t.Helper()
	pki := signertest.NewPKI(t)
	priv, _, leafDER := pki.IssueLeaf(t, leafOpts)
	pemPath := signertest.WriteCombinedPEM(t, t.TempDir(), priv, leafDER)
	s, err := NewSigner(&Config{PEMFile: pemPath})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s, pki
}

// newTestVerifier builds a Verifier trusting trustPKI's CA root.
// Empty cfg.ExpectedSAN defaults to testSAN; cfg.CARootPEMFile is always overwritten.
func newTestVerifier(t *testing.T, trustPKI *signertest.PKI, cfg VerifierConfig) *Verifier {
	t.Helper()
	cfg.CARootPEMFile = signertest.WriteCAPEMFile(t, t.TempDir(), trustPKI.CACertPEM)
	if cfg.ExpectedSAN == "" {
		cfg.ExpectedSAN = testSAN
	}
	v, err := NewVerifier(&cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

func newSignedRequest(t *testing.T, s *Signer, body []byte, signedAt time.Time) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if err := s.SignHTTPRequest(req, body, signedAt); err != nil {
		t.Fatalf("SignHTTPRequest: %v", err)
	}
	return req
}

func assertVerifyError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("expected error containing %q, got: %v", want, err)
	}
}

func TestVerifier_RejectsBadChain(t *testing.T) {
	s, _ := newTestSigner(t, signertest.DefaultLeafOptions(testSAN))
	v := newTestVerifier(t, signertest.NewPKI(t), VerifierConfig{})
	body := []byte("{}")
	err := v.VerifyHTTPRequest(newSignedRequest(t, s, body, time.Now()), body)
	assertVerifyError(t, err, "verify chain")
}

func TestVerifier_RejectsExpiredCert(t *testing.T) {
	opts := signertest.DefaultLeafOptions(testSAN)
	opts.NotBefore = time.Now().Add(-2 * time.Hour)
	opts.NotAfter = time.Now().Add(-time.Hour)
	s, pki := newTestSigner(t, opts)
	v := newTestVerifier(t, pki, VerifierConfig{})
	body := []byte("{}")
	signTime := opts.NotBefore.Add(time.Minute) // within cert validity so sender signs; verifier sees real now > NotAfter
	err := v.VerifyHTTPRequest(newSignedRequest(t, s, body, signTime), body)
	assertVerifyError(t, err, "verify chain")
}

func TestVerifier_RejectsTimestampSkew(t *testing.T) {
	s, pki := newTestSigner(t, signertest.DefaultLeafOptions(testSAN))
	body := []byte("{}")
	for _, tc := range []struct {
		name   string
		offset time.Duration
	}{
		{"past", -10 * time.Minute},
		{"future", 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			v := newTestVerifier(t, pki, VerifierConfig{
				TimestampSkew: time.Minute,
				Now:           func() time.Time { return now },
			})
			err := v.VerifyHTTPRequest(newSignedRequest(t, s, body, now.Add(tc.offset)), body)
			assertVerifyError(t, err, "timestamp outside tolerance")
		})
	}
}

func TestVerifier_RejectsWrongLengthSignature(t *testing.T) {
	s, pki := newTestSigner(t, signertest.DefaultLeafOptions(testSAN))
	v := newTestVerifier(t, pki, VerifierConfig{})
	body := []byte("{}")
	req := newSignedRequest(t, s, body, time.Now())
	req.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString([]byte("too-short")))
	assertVerifyError(t, v.VerifyHTTPRequest(req, body), "signature has wrong length")
}

func TestVerifier_RejectsWrongSAN(t *testing.T) {
	s, pki := newTestSigner(t, signertest.DefaultLeafOptions("https://webhook-signer.testnet.arbitrum.internal"))
	v := newTestVerifier(t, pki, VerifierConfig{
		ExpectedSAN: "https://webhook-signer.mainnet.arbitrum.internal",
	})
	body := []byte("{}")
	err := v.VerifyHTTPRequest(newSignedRequest(t, s, body, time.Now()), body)
	assertVerifyError(t, err, "SAN does not contain expected URI")
}

func TestVerifier_RejectsMissingDigitalSignatureKeyUsage(t *testing.T) {
	opts := signertest.DefaultLeafOptions(testSAN)
	opts.KeyUsage = x509.KeyUsage(0)
	s, pki := newTestSigner(t, opts)
	v := newTestVerifier(t, pki, VerifierConfig{})
	body := []byte("{}")
	err := v.VerifyHTTPRequest(newSignedRequest(t, s, body, time.Now()), body)
	assertVerifyError(t, err, "missing DigitalSignature key usage")
}

func TestVerifier_RejectsMissingHeader(t *testing.T) {
	s, pki := newTestSigner(t, signertest.DefaultLeafOptions(testSAN))
	v := newTestVerifier(t, pki, VerifierConfig{})
	body := []byte("{}")
	for _, header := range []string{HeaderSignature, HeaderSignatureCert, HeaderSignatureTimestamp} {
		t.Run(header, func(t *testing.T) {
			req := newSignedRequest(t, s, body, time.Now())
			req.Header.Del(header)
			assertVerifyError(t, v.VerifyHTTPRequest(req, body), "missing signature headers")
		})
	}
}

func TestNewVerifier_RejectsNegativeTimestampSkew(t *testing.T) {
	pki := signertest.NewPKI(t)
	caPath := signertest.WriteCAPEMFile(t, t.TempDir(), pki.CACertPEM)
	_, err := NewVerifier(&VerifierConfig{
		CARootPEMFile: caPath,
		ExpectedSAN:   testSAN,
		TimestampSkew: -time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "timestamp-skew must be >= 0") {
		t.Fatalf("expected negative-skew error, got: %v", err)
	}
}

func TestVerifier_RejectsTamperedBody(t *testing.T) {
	s, pki := newTestSigner(t, signertest.DefaultLeafOptions(testSAN))
	v := newTestVerifier(t, pki, VerifierConfig{})
	body := []byte(`{"event":"original"}`)
	req := newSignedRequest(t, s, body, time.Now())
	tampered := []byte(`{"event":"tampered"}`)
	assertVerifyError(t, v.VerifyHTTPRequest(req, tampered), "signature verification failed")
}
