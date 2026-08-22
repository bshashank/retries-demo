package api

import (
	"bytes"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

const indexFile = "index.html"

// spaHandler serves the embedded single-page app.
//
// Fallback rule: a request for a path that does not exist gets index.html so
// client-side routes survive a hard refresh — but only if it does not look like
// an asset. A missing /assets/index-abc123.js must 404; answering it with HTML
// produces the classic "Unexpected token '<'" console error and hides the real
// problem (a stale or failed frontend build).
func spaHandler(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "method "+r.Method+" not allowed on "+r.URL.Path)
			return
		}

		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" || name == "." {
			name = indexFile
		}

		if fs.ValidPath(name) {
			if info, err := fs.Stat(fsys, name); err == nil && !info.IsDir() {
				serveFile(w, r, fsys, name)
				return
			}
		}

		if looksLikeAsset(name) {
			http.NotFound(w, r)
			return
		}
		serveFile(w, r, fsys, indexFile)
	})
}

// looksLikeAsset reports whether a miss should 404 rather than fall back to the
// SPA shell: anything under a build-output directory, or any path with a file
// extension (client routes are extensionless).
func looksLikeAsset(name string) bool {
	if strings.HasPrefix(name, "assets/") || strings.HasPrefix(name, "static/") {
		return true
	}
	return path.Ext(name) != ""
}

func serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) {
	f, err := fsys.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	// Vite emits content-hashed asset filenames, so they are immutable; the
	// shell must never be cached or users get pinned to a stale bundle.
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}

	// ServeContent derives Content-Type from the extension and handles range
	// requests and conditional GETs. embed.FS reports a zero ModTime, which it
	// correctly treats as "no Last-Modified".
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, info.ModTime(), rs)
		return
	}
	body, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "failed to read asset", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, name, info.ModTime(), bytes.NewReader(body))
}
