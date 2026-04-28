// Copyright 2026, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

package signer

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

func parseCombinedPEM(data []byte) (*credentials, error) {
	var privateKey ed25519.PrivateKey
	var leaf *x509.Certificate

	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch block.Type {
		case "PRIVATE KEY":
			if privateKey != nil {
				continue
			}
			key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("parse PKCS#8 private key: %w", err)
			}
			ed, ok := key.(ed25519.PrivateKey)
			if !ok {
				return nil, fmt.Errorf("private key is not Ed25519 (got %T)", key)
			}
			privateKey = ed
		case "CERTIFICATE":
			if leaf != nil {
				continue
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("parse certificate: %w", err)
			}
			leaf = cert
		}
	}

	if privateKey == nil {
		return nil, errors.New("no PRIVATE KEY block found in PEM")
	}
	if leaf == nil {
		return nil, errors.New("no CERTIFICATE block found in PEM")
	}
	if leaf.IsCA {
		return nil, errors.New("first certificate in PEM is a CA, expected leaf")
	}

	leafPub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("leaf certificate public key is not Ed25519 (got %T)", leaf.PublicKey)
	}
	if !privateKey.Public().(ed25519.PublicKey).Equal(leafPub) {
		return nil, errors.New("private key does not match leaf certificate public key")
	}

	return &credentials{
		privateKey: privateKey,
		leafDER:    leaf.Raw,
	}, nil
}
