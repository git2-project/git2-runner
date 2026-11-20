package inbox

import (
	"strings"
	"testing"
	"time"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
)

func TestBackoff(t *testing.T) {
	s := &Service{opts: DefaultOptions()}
	for attempts, want := range map[int]time.Duration{
		1: 5 * time.Second, 2: 10 * time.Second, 3: 20 * time.Second,
		7: 5 * time.Minute, 1000: 5 * time.Minute,
	} {
		if got := s.backoff(attempts); got != want {
			t.Errorf("backoff(%d) = %v, want %v", attempts, got, want)
		}
	}
}

func TestStartsPipeline(t *testing.T) {
	const sha = "9fceb02d0ae598e95dc970b74767f19372d61af8"
	const zero = "0000000000000000000000000000000000000000"
	for _, c := range []struct {
		ref, newSHA string
		want        bool
	}{
		{"refs/heads/main", sha, true},
		{"refs/tags/v1.0.0", sha, true},
		{"refs/heads/main", zero, false}, // deleted
		{"refs/heads/main", "", false},
		{"refs/notes/commits", sha, false},
		{"refs/merge-requests/1/head", sha, false},
		{"main", sha, false},
		{"refs/heads/" + strings.Repeat("x", 244), sha, true}, // 255 bytes
		{"refs/heads/" + strings.Repeat("x", 245), sha, false},
	} {
		got := startsPipeline(&gitv1.RefUpdate{Ref: c.ref, NewSha: c.newSHA})
		if got != c.want {
			t.Errorf("startsPipeline(%.30q, %.8q) = %v, want %v", c.ref, c.newSHA, got, c.want)
		}
	}
}
