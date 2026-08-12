package state

import (
	"encoding/base32"
	"strings"
	"testing"
)

func TestNewWorkspaceIDRoundTrip(t *testing.T) {
	seen := make(map[string]struct{})
	for range 128 {
		id, err := NewWorkspaceID()
		if err != nil {
			t.Fatal(err)
		}
		if !idRE.MatchString(id) {
			t.Fatalf("generated invalid workspace ID %q", id)
		}
		decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(id))
		if err != nil {
			t.Fatalf("decode %q: %v", id, err)
		}
		if len(decoded) != 16 {
			t.Fatalf("decoded ID has %d bytes, want 16", len(decoded))
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate generated ID %q", id)
		}
		seen[id] = struct{}{}
	}
}
