package web

import (
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"
)

// The entry document names the current hashed bundles, so browsers must revalidate it: a tab
// opened after an upgrade then loads the new build. Hashed assets keep their caching, and a
// stricter header set before this handler stays.
func TestEntryDocumentRevalidatesAndAssetsKeepCaching(t *testing.T) {
	asset := ""
	_ = fs.WalkDir(distFS, "dist/assets", func(path string, d fs.DirEntry, err error) error {
		if err == nil && asset == "" && strings.HasSuffix(path, ".js") {
			asset = strings.TrimPrefix(path, "dist")
		}
		return nil
	})
	if asset == "" {
		t.Skip("web/dist is not built")
	}
	get := func(path, preset string) string {
		rec := httptest.NewRecorder()
		if preset != "" {
			rec.Header().Set("Cache-Control", preset)
		}
		Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		return rec.Header().Get("Cache-Control")
	}
	paths := []string{"/", "/settings/appearance"}
	pages, _ := fs.Glob(distFS, "dist/*.html")
	for _, page := range pages {
		if page != "dist/index.html" { // index.html redirects to "/"
			paths = append(paths, strings.TrimPrefix(page, "dist"))
		}
	}
	for _, path := range paths {
		if got := get(path, ""); got != "no-cache" {
			t.Errorf("%s: Cache-Control %q, want no-cache", path, got)
		}
	}
	if got := get(asset, ""); got != "" {
		t.Errorf("%s: Cache-Control %q, want none (unchanged)", asset, got)
	}
	if got := get("/g/shared-link", "no-store"); got != "no-store" {
		t.Errorf("a stricter Cache-Control was replaced: %q", got)
	}
}
