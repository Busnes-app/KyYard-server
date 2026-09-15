package backup_test

import (
	"path/filepath"
	"testing"

	"github.com/Busness-app/ky-primitives/recoveryclient/guardtest"
)

// Nothing in the server opens a capsule sealed to the suite key, combines shares, or rebuilds
// the key from a seed. The one exemption is the restore command, with shares typed by an
// operator; the drill opens only a capsule sealed to a key it made and discarded, inside the lib.
func TestNothingInTheServerDecrypts(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	guardtest.NoDecryptOutside(t, root, map[string][]string{
		filepath.Join("cmd", "server", "main.go"): {"restore"},
	})
}
