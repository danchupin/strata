// Package strata embeds the React console (web/dist) and exposes an
// http.Handler that serves it under /console/ with SPA fallback.
//
// The embed pattern requires web/dist to exist at build time. Run
// `make web-build` (or the top-level `make build`, which depends on it)
// to populate it; an empty dist will fail the Go build with a clear
// "pattern web/dist: no matching files found" error from the embed
// package.
package strata

import (
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:web/dist
var consoleFS embed.FS

// ConsoleFS returns the embedded console filesystem rooted at web/dist.
// Empty if the bundle was not built.
func ConsoleFS() fs.FS {
	sub, err := fs.Sub(consoleFS, "web/dist")
	if err != nil {
		return consoleFS
	}
	return sub
}

// assetsList returns base-names of files under web/dist/assets.
func assetsList(root fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(root, "assets")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// ConsoleHandler serves the embedded console under /console/.
//
// Requests for static asset paths (anything with an extension that exists
// in the bundle) are served directly. All other paths fall back to
// index.html so the SPA router can take over (deep links, refreshes).
func ConsoleHandler() http.Handler {
	root := ConsoleFS()
	fileServer := http.FileServer(http.FS(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip the /console prefix so file paths line up with embed root.
		rel := strings.TrimPrefix(r.URL.Path, "/console")
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			rel = "index.html"
		}

		if served := serveIfExists(w, r, root, rel); served {
			return
		}

		// SPA fallback: serve index.html for unknown client-side routes.
		_ = fileServer // retained for future static-prefix needs
		serveIndex(w, r, root)
	})
}

func serveIfExists(w http.ResponseWriter, r *http.Request, root fs.FS, rel string) bool {
	f, err := root.Open(rel)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return false
	}
	http.ServeFileFS(w, r, root, rel)
	return true
}

func serveIndex(w http.ResponseWriter, r *http.Request, root fs.FS) {
	const idx = "index.html"
	f, err := root.Open(idx)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "console bundle not built — run `make web-build`", http.StatusInternalServerError)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = f.Close()
	// Avoid caching index.html so rolling deploys pick up new asset hashes.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeFileFS(w, r, root, path.Clean(idx))
}
