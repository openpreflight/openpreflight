// SPDX-License-Identifier: Apache-2.0

// Package githubapp authenticates as a GitHub App: App JWT for app-level calls,
// installation access tokens for repo-level ones (Checks, clone).
package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ParsePrivateKey accepts the PEM GitHub hands out (PKCS#1) and the PKCS#8 form
// some tooling re-encodes it into.
func ParsePrivateKey(pemData string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, errors.New("githubapp: not a PEM private key (expected a -----BEGIN ... PRIVATE KEY----- block)")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("githubapp: parse private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("githubapp: private key is %T, GitHub Apps use RSA", parsed)
	}
	return key, nil
}

// jwtTTL is how long an App JWT is valid. GitHub allows 10 minutes; 9 leaves
// room for clock skew, which GitHub rejects harder than expiry.
const jwtTTL = 9 * time.Minute

// AppJWT mints a signed App JWT for app-level endpoints.
//
// issuedAt is a parameter so tests are deterministic; callers pass time.Now().
func AppJWT(appID int64, key *rsa.PrivateKey, issuedAt time.Time) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	// iat is backdated 60s: GitHub rejects a JWT whose iat is in the future by
	// even a second, and container clocks drift.
	claims := map[string]any{
		"iat": issuedAt.Add(-60 * time.Second).Unix(),
		"exp": issuedAt.Add(jwtTTL).Unix(),
		"iss": strconv.FormatInt(appID, 10),
	}
	h, err := jsonSegment(header)
	if err != nil {
		return "", err
	}
	c, err := jsonSegment(claims)
	if err != nil {
		return "", err
	}
	signing := h + "." + c
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("githubapp: sign jwt: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func jsonSegment(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("githubapp: encode jwt segment: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
