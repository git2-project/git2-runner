// Package token creates the secret tokens ci-svc hands out and the hashes
// it stores instead of them.
//
// A token is a prefix plus 32 random bytes in base64url. The prefix tells
// people (and secret scanners) what a leaked token is. Only the SHA-256 of
// a token is stored; a random 256-bit token needs no slow hash.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// Token prefixes.
const (
	// RunnerPrefix starts a runner token, which authenticates a runner.
	RunnerPrefix = "g2r_"
	// RegistrationPrefix starts a one-time runner registration token.
	RegistrationPrefix = "g2rt_"
)

// New returns a new random token with the given prefix.
func New(prefix string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never returns an error
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// Hash returns the hex SHA-256 of a token, as stored in the database.
func Hash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// HasPrefix reports whether tok looks like a token with prefix. It only
// filters out obvious garbage before a database lookup.
func HasPrefix(tok, prefix string) bool {
	return strings.HasPrefix(tok, prefix) && len(tok) == len(prefix)+43
}
