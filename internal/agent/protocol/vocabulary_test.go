package protocol

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The web's step-code and unsupported-code tables are checked against this vocabulary through
// a generated fixture. KY_UPDATE_FIXTURES=1 rewrites it.
func TestWebVocabularyFixture(t *testing.T) {
	want, err := json.MarshalIndent(map[string][]string{"step_codes": slices.Sorted(maps.Keys(stepCodes)), "unsupported_codes": UnsupportedCodes}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	path := filepath.Join("..", "..", "..", "web", "src", "protocol-codes.json")
	if os.Getenv("KY_UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s is stale (%v): run KY_UPDATE_FIXTURES=1 go test ./internal/agent/protocol -run TestWebVocabularyFixture", path, err)
	}
}
