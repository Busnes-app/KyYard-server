package migrations

import "testing"

func TestLatestIsTheLastRegisteredVersion(t *testing.T) {
	if got, want := Latest(), registry[len(registry)-1].Version; got != want {
		t.Fatalf("Latest() = %d, want %d", got, want)
	}
	for _, m := range registry {
		if m.Version > Latest() {
			t.Fatalf("version %d exceeds Latest() %d", m.Version, Latest())
		}
	}
}
