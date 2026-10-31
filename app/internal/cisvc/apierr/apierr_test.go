package apierr

import (
	"fmt"
	"testing"
)

func TestParseID(t *testing.T) {
	if _, err := ParseID("id", "3f2b8c1e-1d6a-4b5c-9e8f-0a1b2c3d4e5f"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "x", "3f2b8c1e1d6a4b5c9e8f0a1b2c3d4e5f", "{3f2b8c1e-1d6a-4b5c-9e8f-0a1b2c3d4e5f}"} {
		_, err := ParseID("id", bad)
		if KindOf(err) != Invalid {
			t.Errorf("ParseID(%q) = %v, want Invalid", bad, err)
		}
	}
	if KindOf(fmt.Errorf("wrapped: %w", New(NotFound, "x"))) != NotFound {
		t.Error("KindOf must see wrapped errors")
	}
}
