// Copyright 2026, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

package signer

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/offchainlabs/nitro/cmd/filtering-report/signer/signertest"
)

func assertParseError(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("expected error %v, got: %v", want, got)
	}
}

func TestParseCombinedPEM_RejectsMismatchedKeyAndCert(t *testing.T) {
	pki := signertest.NewPKI(t)
	priv1, _, _ := pki.IssueLeaf(t, signertest.DefaultLeafOptions(testSAN))
	_, _, leafDER2 := pki.IssueLeaf(t, signertest.DefaultLeafOptions(testSAN))

	keyPEM, certPEM := signertest.EncodePEMBundle(t, priv1, leafDER2)
	bundle := slices.Concat(keyPEM, certPEM)

	_, err := parseCombinedPEM(bundle)
	assertParseError(t, err, errKeyCertMismatch)
}

func TestParseCombinedPEM_RejectsKeyOnly(t *testing.T) {
	pki := signertest.NewPKI(t)
	priv, _, leafDER := pki.IssueLeaf(t, signertest.DefaultLeafOptions(testSAN))
	keyPEM, _ := signertest.EncodePEMBundle(t, priv, leafDER)

	_, err := parseCombinedPEM(keyPEM)
	assertParseError(t, err, errMissingCertificate)
}

func TestParseCombinedPEM_RejectsCertOnly(t *testing.T) {
	pki := signertest.NewPKI(t)
	priv, _, leafDER := pki.IssueLeaf(t, signertest.DefaultLeafOptions(testSAN))
	_, certPEM := signertest.EncodePEMBundle(t, priv, leafDER)

	_, err := parseCombinedPEM(certPEM)
	assertParseError(t, err, errMissingPrivateKey)
}

func TestParseCombinedPEM_RejectsDuplicatePrivateKey(t *testing.T) {
	pki := signertest.NewPKI(t)
	priv, _, leafDER := pki.IssueLeaf(t, signertest.DefaultLeafOptions(testSAN))
	keyPEM, certPEM := signertest.EncodePEMBundle(t, priv, leafDER)
	bundle := slices.Concat(keyPEM, keyPEM, certPEM)

	_, err := parseCombinedPEM(bundle)
	assertParseError(t, err, errDuplicatePrivateKey)
}

func TestParseCombinedPEM_RejectsCAAsLeaf(t *testing.T) {
	pki := signertest.NewPKI(t)
	keyPEM, certPEM := signertest.EncodePEMBundle(t, pki.CAPriv, pki.CACertDER)
	bundle := slices.Concat(keyPEM, certPEM)

	_, err := parseCombinedPEM(bundle)
	assertParseError(t, err, errLeafIsCA)
}

func TestParseCombinedPEM_RejectsUnsupportedBlockType(t *testing.T) {
	bundle := []byte("-----BEGIN EC PRIVATE KEY-----\nQUJD\n-----END EC PRIVATE KEY-----\n")
	_, err := parseCombinedPEM(bundle)
	if err == nil || !strings.Contains(err.Error(), "unsupported PEM block type") {
		t.Fatalf("expected unsupported-block-type error, got: %v", err)
	}
}
