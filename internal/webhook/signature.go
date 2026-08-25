// SPDX-License-Identifier: Apache-2.0

// Package webhook verifies GitHub App webhook deliveries and normalises the
// events we act on (check_suite, check_run).
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// ErrBadSignature means the body did not match X-Hub-Signature-256 for this
// App's secret. Never say more than that to the caller.
var ErrBadSignature = errors.New("webhook: signature mismatch")

// VerifySignature checks the sha256 HMAC of the raw body. The raw bytes matter:
// re-serialising the JSON changes the digest.
func VerifySignature(secret string, body []byte, header string) error {
	if secret == "" {
		return errors.New("webhook: no webhook secret configured for this App")
	}
	if !strings.HasPrefix(header, "sha256=") {
		return ErrBadSignature
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return ErrBadSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), want) {
		return ErrBadSignature
	}
	return nil
}
