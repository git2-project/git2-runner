// Package secretbox encrypts CI secret values with AES-256-GCM.
//
// The key comes from configuration (GIT2_CI_SECRETS_KEY), never from the
// database, so a database dump alone reveals no secret. Every value gets a
// random nonce. The caller passes additional data that binds a ciphertext
// to its row (scope, scope ID and name), so a ciphertext copied into
// another row does not decrypt.
//
// Each ciphertext is stored with the ID of the key that made it. After a
// restore with the wrong key, ci-svc can name the problem at startup
// instead of failing on every job.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the size of a key in bytes.
const KeySize = 32

// ErrWrongKey means a value was encrypted with a different key.
var ErrWrongKey = errors.New("secret was encrypted with a different key")

// Box encrypts and decrypts with one key.
type Box struct {
	aead  cipher.AEAD
	keyID string
}

// ParseKey decodes a key written in standard or URL-safe base64, with or
// without padding, as `openssl rand -base64 32` prints it. It never puts
// the key into its error.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := enc.DecodeString(s); err == nil {
			if len(key) != KeySize {
				return nil, fmt.Errorf("the key must be %d bytes, got %d", KeySize, len(key))
			}
			return key, nil
		}
	}
	return nil, errors.New("the key is not base64")
}

// New returns a Box for a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("the key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// The key ID is a hash of a hash: it identifies the key without
	// helping anyone guess it.
	inner := sha256.Sum256(key)
	outer := sha256.Sum256(append([]byte("git2 ci secrets key id:"), inner[:]...))
	return &Box{aead: aead, keyID: "k1:" + hex.EncodeToString(outer[:6])}, nil
}

// KeyID names the key, for the key_id column.
func (b *Box) KeyID() string { return b.keyID }

// Seal encrypts plaintext. It returns the ciphertext and the nonce.
func (b *Box) Seal(plaintext, aad []byte) (ciphertext, nonce []byte) {
	nonce = make([]byte, b.aead.NonceSize())
	_, _ = rand.Read(nonce) // never returns an error
	return b.aead.Seal(nil, nonce, plaintext, aad), nonce
}

// Open decrypts a value that Seal made with the key named keyID.
func (b *Box) Open(ciphertext, nonce, aad []byte, keyID string) ([]byte, error) {
	if keyID != b.keyID {
		return nil, ErrWrongKey
	}
	if len(nonce) != b.aead.NonceSize() {
		return nil, errors.New("decrypt secret: bad nonce")
	}
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, errors.New("decrypt secret: authentication failed")
	}
	return plaintext, nil
}
