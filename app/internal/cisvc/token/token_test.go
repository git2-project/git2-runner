package token

import "testing"

func TestToken(t *testing.T) {
	a, b := New(RunnerPrefix), New(RunnerPrefix)
	if a == b {
		t.Fatal("tokens must be random")
	}
	if !HasPrefix(a, RunnerPrefix) || HasPrefix(a, RegistrationPrefix) || HasPrefix(a[:len(a)-1], RunnerPrefix) {
		t.Errorf("HasPrefix(%q) is wrong", a)
	}
	if h := Hash(a); len(h) != 64 || h == Hash(b) || h != Hash(a) {
		t.Errorf("Hash = %q", h)
	}
}
