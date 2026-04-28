// Copyright 2026, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

package signer

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/spf13/pflag"

	"github.com/ethereum/go-ethereum/log"

	"github.com/offchainlabs/nitro/util/stopwaiter"
)

const reloadInterval = time.Hour

const (
	HeaderSignature          = "X-Signature"
	HeaderSignatureCert      = "X-Signature-Cert"
	HeaderSignatureTimestamp = "X-Signature-Timestamp"
)

type Config struct {
	PEMFile string `koanf:"pem-file"`
}

var DefaultConfig = Config{
	PEMFile: "",
}

func ConfigAddOptions(prefix string, f *pflag.FlagSet) {
	f.String(prefix+".pem-file", DefaultConfig.PEMFile, "path to combined PEM file containing Ed25519 private key and leaf certificate; empty disables signing")
}

func (c *Config) Enabled() bool {
	return c.PEMFile != ""
}

func (c *Config) Validate() error {
	return nil
}

type credentials struct {
	privateKey ed25519.PrivateKey
	leafDER    []byte
}

type Signer struct {
	stopwaiter.StopWaiter
	pemFile string
	creds   atomic.Pointer[credentials]
}

func NewSigner(config *Config) (*Signer, error) {
	if config == nil {
		return nil, errors.New("config must not be nil")
	}
	if !config.Enabled() {
		return nil, errors.New("signer is disabled (empty pem-file)")
	}
	s := &Signer{pemFile: config.PEMFile}
	if err := s.reloadConfig(); err != nil {
		return nil, fmt.Errorf("initial PEM load: %w", err)
	}
	return s, nil
}

func (s *Signer) Start(ctx context.Context) {
	s.StopWaiter.Start(ctx, s)
	s.CallIteratively(func(_ context.Context) time.Duration {
		if err := s.reloadConfig(); err != nil {
			log.Warn("Failed to reload signing PEM, retaining previous credentials", "err", err, "file", s.pemFile)
		} else {
			log.Info("Reloaded signing PEM", "file", s.pemFile)
		}
		return reloadInterval
	})
}

func (s *Signer) reloadConfig() error {
	data, err := os.ReadFile(s.pemFile)
	if err != nil {
		return fmt.Errorf("read PEM file %q: %w", s.pemFile, err)
	}
	creds, err := parseCombinedPEM(data)
	if err != nil {
		return err
	}
	s.creds.Store(creds)
	return nil
}

func (s *Signer) SignHTTPRequest(req *http.Request, body []byte, now time.Time) error {
	creds := s.creds.Load()
	if creds == nil {
		return errors.New("signer has no credentials loaded")
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	payload := buildSigningPayload(timestamp, body)
	signature := ed25519.Sign(creds.privateKey, payload)

	req.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(signature))
	req.Header.Set(HeaderSignatureCert, base64.StdEncoding.EncodeToString(creds.leafDER))
	req.Header.Set(HeaderSignatureTimestamp, timestamp)
	return nil
}

func buildSigningPayload(timestamp string, body []byte) []byte {
	payload := make([]byte, 0, len(timestamp)+1+len(body))
	payload = append(payload, timestamp...)
	payload = append(payload, '.')
	payload = append(payload, body...)
	return payload
}
