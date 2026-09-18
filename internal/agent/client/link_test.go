package client_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
)

func TestEnrollmentLink(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	link := "https://yard.example:9443/#kyyard=" + token
	origin, parsed, err := client.ParseLink(link)
	if err != nil || origin != "https://yard.example:9443" || parsed != token {
		t.Fatal("valid link refused")
	}
	for _, bad := range []string{
		"http://yard.example/#kyyard=" + token, "https://user:secret@yard.example/#kyyard=" + token,
		"https://yard.example/path#kyyard=" + token, "https://yard.example/?token=secret#kyyard=" + token,
		"https://yard.example/#kyyard=short", "https:///#kyyard=" + token, "%", strings.Repeat("x", 4097),
	} {
		_, _, err := client.ParseLink(bad)
		if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid link accepted or leaked")
		}
	}
}
