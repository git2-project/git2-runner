package runner

import (
	"strings"
	"testing"
)

func TestMasker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		writes []string
		want   string
	}{
		{"whole", []string{"hunter22"}, []string{"pw=hunter22\n"}, "pw=[MASKED]\n"},
		{"split across writes", []string{"hunter22"}, []string{"pw=hun", "ter", "22 ok\n"}, "pw=[MASKED] ok\n"},
		{"twice", []string{"abcd"}, []string{"abcdabcd"}, "[MASKED][MASKED]"},
		{"prefix then other text", []string{"hunter22"}, []string{"hunt", "ing\n"}, "hunting\n"},
		{"too short to mask", []string{"on", "1"}, []string{"on 1\n"}, "on 1\n"},
		{"longest first", []string{"secret", "secret-and-more"}, []string{"secret-and-more secret"}, "[MASKED] [MASKED]"},
		{"lines of a key", []string{"-----BEGIN KEY-----\nAAAABBBB\n-----END KEY-----"}, []string{"key line: AAAABBBB\n"}, "key line: [MASKED]\n"},
		{"no secrets", nil, []string{"plain"}, "plain"},
		{"held tail released on close", []string{"hunter22"}, []string{"ends with hunt"}, "ends with hunt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMasker(tc.values)
			var out strings.Builder
			for _, w := range tc.writes {
				out.Write(m.write([]byte(w)))
			}
			out.Write(m.close())
			if out.String() != tc.want {
				t.Errorf("got %q, want %q", out.String(), tc.want)
			}
		})
	}
}

// TestMaskerReleasesSafeText checks that text which cannot start a secret
// is not held back, so log lines reach the user without waiting.
func TestMaskerReleasesSafeText(t *testing.T) {
	m := newMasker([]string{"hunter22"})
	if got := string(m.write([]byte("building...\n"))); got != "building...\n" {
		t.Errorf("held back %q", got)
	}
	if got := string(m.write([]byte("value: hu"))); got != "value: " {
		t.Errorf("got %q, want the possible start of the secret held back", got)
	}
}
