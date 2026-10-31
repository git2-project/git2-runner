package page

import "testing"

func TestSize(t *testing.T) {
	for in, want := range map[int32]int{0: 20, 1: 1, 100: 100, 101: 100, 5000: 100} {
		if got, err := Size(in, 20); err != nil || got != want {
			t.Errorf("Size(%d) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := Size(-1, 20); err == nil {
		t.Error("negative size accepted")
	}
}

func TestToken(t *testing.T) {
	type cur struct{ A string }
	tok := Encode(cur{A: "x"})
	var got cur
	if ok, err := Decode(tok, &got); !ok || err != nil || got.A != "x" {
		t.Fatalf("Decode = %v %v %+v", ok, err, got)
	}
	if ok, err := Decode("", &got); ok || err != nil {
		t.Errorf("empty token: %v %v", ok, err)
	}
	for _, bad := range []string{"!!", "bm90IGpzb24"} {
		if _, err := Decode(bad, &got); err == nil {
			t.Errorf("Decode(%q) accepted", bad)
		}
	}
}
