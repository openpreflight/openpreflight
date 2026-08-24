// Package secret encrypts the secret columns (GitHub PEMs, webhook secrets,
// Coolify API tokens) so a stolen ci.db is not a full compromise.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Box seals and opens values with AES-256-GCM under a key derived from
// CI_SECRET_KEY. Set CI_SECRET_KEY_OLD on boot to re-seal stored columns.
type Box struct {
	aead cipher.AEAD
}

// prefix marks a sealed value so we can tell ciphertext from a legacy plaintext
// column and fail loudly rather than handing a PEM-shaped blob to crypto/rsa.
const prefix = "v1:"

// ErrNotSealed means the stored value was not produced by Seal.
var ErrNotSealed = errors.New("secret: value is not sealed")

// New derives the box key from the configured secret. The raw env value is any
// length; SHA-256 gives us the 32 bytes AES-256 needs.
func New(rawKey string) (*Box, error) {
	if strings.TrimSpace(rawKey) == "" {
		return nil, errors.New("secret: empty key")
	}
	sum := sha256.Sum256([]byte(rawKey))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("secret: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: new gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext into a storable string. Empty in, empty out, so an
// optional secret column stays empty rather than holding an encrypted "".
func (b *Box) Seal(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("secret: nonce: %w", err)
	}
	ct := b.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return prefix + base64.StdEncoding.EncodeToString(ct), nil
}

// Open decrypts a value produced by Seal.
func (b *Box) Open(sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	if !strings.HasPrefix(sealed, prefix) {
		return "", ErrNotSealed
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, prefix))
	if err != nil {
		return "", fmt.Errorf("secret: decode: %w", err)
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("secret: ciphertext too short")
	}
	pt, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		// Wrong CI_SECRET_KEY looks exactly like this.
		return "", fmt.Errorf("secret: open (wrong CI_SECRET_KEY?): %w", err)
	}
	return string(pt), nil
}

// Redact renders a secret for a GET response: enough to recognise, not enough
// to use.
func Redact(plaintext string) string {
	switch {
	case plaintext == "":
		return ""
	case len(plaintext) <= 8:
		return "…"
	default:
		return plaintext[:4] + "…" + fmt.Sprintf("(%d bytes)", len(plaintext))
	}
}
