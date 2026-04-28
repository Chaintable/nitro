// Copyright 2026, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

package signer

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/offchainlabs/nitro/cmd/filtering-report/signer/signertest"
)

func newSignedRequest(t *testing.T, s *Signer, body []byte, signedAt time.Time) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if err := s.SignHTTPRequest(req, body, signedAt); err != nil {
		t.Fatalf("SignHTTPRequest: %v", err)
	}
	return req
}

func TestVerifier_RejectsBadChain(t *testing.T) {
	signerPKI := signertest.NewPKI(t)
	otherPKI := signertest.NewPKI(t)
	priv, _, leafDER := signerPKI.IssueLeaf(t, signertest.DefaultLeafOptions(testSAN))
	dir := t.TempDir()
	pemPath := signertest.WriteCombinedPEM(t, dir, priv, leafDER)
	s, err := NewSigner(&Config{PEMFile: pemPath})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	v, err := NewVerifier(&VerifierConfig{
		CARootPEMFile: signertest.WriteCAPEMFile(t, dir, otherPKI.CACertPEM),
		ExpectedSAN:   testSAN,
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	body := []byte("{}")
	err = v.VerifyHTTPRequest(newSignedRequest(t, s, body, time.Now()), body)
	if err == nil || !strings.Contains(err.Error(), "verify chain") {
		t.Fatalf("expected chain verification error, got: %v", err)
	}
}

func TestVerifier_RejectsExpiredCert(t *testing.T) {
	pki := signertest.NewPKI(t)
	opts := signertest.DefaultLeafOptions(testSAN)
	opts.NotBefore = time.Now().Add(-2 * time.Hour)
	opts.NotAfter = time.Now().Add(-time.Hour)
	priv, _, leafDER := pki.IssueLeaf(t, opts)
	dir := t.TempDir()
	pemPath := signertest.WriteCombinedPEM(t, dir, priv, leafDER)
	s, err := NewSigner(&Config{PEMFile: pemPath})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	v, err := NewVerifier(&VerifierConfig{
		CARootPEMFile: signertest.WriteCAPEMFile(t, dir, pki.CACertPEM),
		ExpectedSAN:   testSAN,
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	body := []byte("{}")
	err = v.VerifyHTTPRequest(newSignedRequest(t, s, body, time.Now()), body)
	if err == nil {
		t.Fatal("expected expiry error, got nil")
	}
}

func TestVerifier_RejectsTimestampSkew(t *testing.T) {
	pki := signertest.NewPKI(t)
	priv, _, leafDER := pki.IssueLeaf(t, signertest.DefaultLeafOptions(testSAN))
	dir := t.TempDir()
	pemPath := signertest.WriteCombinedPEM(t, dir, priv, leafDER)
	s, err := NewSigner(&Config{PEMFile: pemPath})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	now := time.Now()
	v, err := NewVerifier(&VerifierConfig{
		CARootPEMFile: signertest.WriteCAPEMFile(t, dir, pki.CACertPEM),
		ExpectedSAN:   testSAN,
		TimestampSkew: time.Minute,
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	body := []byte("{}")
	err = v.VerifyHTTPRequest(newSignedRequest(t, s, body, now.Add(-10*time.Minute)), body)
	if err == nil || !strings.Contains(err.Error(), "timestamp outside tolerance") {
		t.Fatalf("expected timestamp skew error, got: %v", err)
	}
}

func TestVerifier_RejectsTamperedBody(t *testing.T) {
	pki := signertest.NewPKI(t)
	priv, _, leafDER := pki.IssueLeaf(t, signertest.DefaultLeafOptions(testSAN))
	dir := t.TempDir()
	pemPath := signertest.WriteCombinedPEM(t, dir, priv, leafDER)
	s, err := NewSigner(&Config{PEMFile: pemPath})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	v, err := NewVerifier(&VerifierConfig{
		CARootPEMFile: signertest.WriteCAPEMFile(t, dir, pki.CACertPEM),
		ExpectedSAN:   testSAN,
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	body := []byte(`{"event":"original"}`)
	req := newSignedRequest(t, s, body, time.Now())
	tampered := []byte(`{"event":"tampered"}`)
	err = v.VerifyHTTPRequest(req, tampered)
	if err == nil || !strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("expected signature failure on tampered body, got: %v", err)
	}
}
