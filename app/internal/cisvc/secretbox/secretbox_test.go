package secretbox

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KeySize)
	_, _ = rand.Read(k)
	return k
}

func TestSealOpen(t *testing.T) {
	box, err := New(newKey(t))
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte("repository\x00id\x00DEPLOY_KEY")
	ct, nonce := box.Seal([]byte("s3cret"), aad)
	if bytes.Contains(ct, []byte("s3cret")) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := box.Open(ct, nonce, aad, box.KeyID())
	if err != nil || string(got) != "s3cret" {
		t.Fatalf("Open = %q, %v", got, err)
	}

	ct2, nonce2 := box.Seal([]byte("s3cret"), aad)
	if bytes.Equal(ct, ct2) || bytes.Equal(nonce, nonce2) {
		t.Error("two seals of one value must differ (random nonce)")
	}
	if _, err := box.Open(ct, nonce, []byte("another row"), box.KeyID()); err == nil {
		t.Error("a ciphertext moved to another row must not decrypt")
	}
	ct[0] ^= 1
	if _, err := box.Open(ct, nonce, aad, box.KeyID()); err == nil {
		t.Error("a modified ciphertext must not decrypt")
	}
}

func TestWrongKey(t *testing.T) {
	key := newKey(t)
	a, _ := New(key)
	again, _ := New(key)
	b, _ := New(newKey(t))
	if a.KeyID() != again.KeyID() {
		t.Error("KeyID must be stable for one key")
	}
	if a.KeyID() == b.KeyID() {
		t.Fatal("different keys must have different IDs")
	}
	ct, nonce := a.Seal([]byte("v"), nil)
	if _, err := b.Open(ct, nonce, nil, a.KeyID()); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("Open with another key = %v, want ErrWrongKey", err)
	}
}

func TestParseKey(t *testing.T) {
	k := newKey(t)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		got, err := ParseKey(" " + enc.EncodeToString(k) + "\n")
		if err != nil || !bytes.Equal(got, k) {
			t.Errorf("ParseKey: %v", err)
		}
	}
	short := base64.StdEncoding.EncodeToString(k[:16])
	if _, err := ParseKey(short); err == nil || strings.Contains(err.Error(), short) {
		t.Errorf("a 16-byte key: %v (the error must not repeat the key)", err)
	}
	if _, err := ParseKey("not base64!"); err == nil {
		t.Error("garbage accepted")
	}
}
