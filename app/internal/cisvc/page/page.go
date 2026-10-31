// Package page implements list pagination per Google AIP-158: page sizes
// with a default and a cap, and opaque page tokens.
package page

import (
	"encoding/base64"
	"encoding/json"

	"github.com/git2-project/git2-runner/app/internal/cisvc/apierr"
)

// MaxSize is the largest page any list RPC returns.
const MaxSize = 100

// Size applies the page size rules: 0 means def, values above MaxSize are
// lowered to it, negative values are rejected.
func Size(n int32, def int) (int, error) {
	switch {
	case n < 0:
		return 0, apierr.InvalidField("page_size", "must not be negative")
	case n == 0:
		return def, nil
	case n > MaxSize:
		return MaxSize, nil
	}
	return int(n), nil
}

// Encode turns a cursor into an opaque token. Clients must not parse it;
// the format can change at any time.
func Encode(cursor any) string {
	b, err := json.Marshal(cursor)
	if err != nil {
		panic("page: cursor is not JSON-encodable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Decode reverses Encode into cursor, which must be a pointer. An empty
// token leaves cursor untouched and returns false.
func Decode(token string, cursor any) (bool, error) {
	if token == "" {
		return false, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || json.Unmarshal(b, cursor) != nil {
		return false, apierr.InvalidField("page_token", "is invalid")
	}
	return true, nil
}
