package main

import (
	"go/parser"
	"go/token"
	"testing"
)

// Update-policy windows are evaluated in IANA zones, and PUT refuses a zone that does not load.
// A bare binary on a host without zoneinfo must behave the same, so the server embeds the zone
// database. Deleting the import fails here.
func TestServerEmbedsTheTimeZoneDatabase(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, im := range f.Imports {
		if im.Path.Value == `"time/tzdata"` && im.Name != nil && im.Name.Name == "_" {
			return
		}
	}
	t.Fatal(`cmd/server does not blank-import "time/tzdata"`)
}
