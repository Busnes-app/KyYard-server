package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed dist/*
var distFS embed.FS

// Handler returns an http.Handler that serves the embedded React PWA frontend with SPA fallback.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			revalidate(w)
			fileServer.ServeHTTP(w, r)
			return
		}

		// Check if file exists in dist
		f, err := sub.Open(path)
		if err == nil {
			_ = f.Close()
			if strings.HasSuffix(path, ".html") {
				revalidate(w)
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		// Fallback to index.html for SPA routing
		revalidate(w)
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}

// revalidate marks an HTML entry document no-cache: it names the current hashed bundles, so a
// tab opened after an upgrade loads the new build. Hashed assets keep their caching, and a
// stricter header set earlier (such as no-store) stays.
func revalidate(w http.ResponseWriter) {
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-cache")
	}
}
