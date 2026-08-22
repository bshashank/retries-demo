package api

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestStaticServesIndexAtRoot(t *testing.T) {
	rec := do(t, newTestRouter(t, newFake()), http.MethodGet, "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<title>shell</title>") {
		t.Errorf("body = %q, want the index shell", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestStaticServesRealAssets(t *testing.T) {
	rec := do(t, newTestRouter(t, newFake()), http.MethodGet, "/assets/app-abc.js", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "console.log('hi')" {
		t.Errorf("body = %q, want the asset contents", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want a javascript type", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable for hashed assets", cc)
	}
}

// A hard refresh on a client-side route must return the shell, not a 404.
func TestSPAFallbackForUnknownRoutes(t *testing.T) {
	for _, path := range []string{"/scenarios", "/node/build", "/deeply/nested/route"} {
		t.Run(path, func(t *testing.T) {
			rec := do(t, newTestRouter(t, newFake()), http.MethodGet, path, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "<title>shell</title>") {
				t.Errorf("body = %q, want the index shell", rec.Body.String())
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
				t.Errorf("Cache-Control = %q, want no-cache for the shell", cc)
			}
		})
	}
}

// A missing asset must 404. Serving HTML instead produces the classic
// "Unexpected token '<'" error and hides a broken frontend build.
func TestMissingAssetsReturn404NotHTML(t *testing.T) {
	for _, path := range []string{
		"/assets/gone.js",
		"/assets/gone.css",
		"/missing.png",
		"/favicon.ico",
		"/some/where/file.woff2",
	} {
		t.Run(path, func(t *testing.T) {
			rec := do(t, newTestRouter(t, newFake()), http.MethodGet, path, "")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "<title>shell</title>") {
				t.Error("missing asset served the SPA shell")
			}
		})
	}
}

func TestStaticRejectsTraversal(t *testing.T) {
	// ServeMux redirects unclean paths, and path.Clean collapses whatever
	// survives; the worst case is an SPA fallback, never a file from outside the
	// embedded tree.
	for _, path := range []string{"/../../etc/passwd", "/assets/../../secret", "//etc/passwd"} {
		t.Run(path, func(t *testing.T) {
			rec := do(t, newTestRouter(t, newFake()), http.MethodGet, path, "")
			switch {
			case rec.Code >= 300 && rec.Code < 400:
				if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/") {
					t.Errorf("redirect Location = %q, want a site-relative path", loc)
				}
			case rec.Code == http.StatusOK || rec.Code == http.StatusNotFound:
				// Shell or miss: both are safe.
			default:
				t.Fatalf("status = %d, want a redirect, 200 (shell), or 404", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "root:") {
				t.Fatal("traversal escaped the embedded filesystem")
			}
		})
	}
}

func TestStaticRejectsWrongMethod(t *testing.T) {
	rec := do(t, newTestRouter(t, newFake()), http.MethodPost, "/", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestNoStaticFSStill404s(t *testing.T) {
	h := NewRouter(newFake(), Options{}) // no Static
	rec := do(t, h, http.MethodGet, "/", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when no frontend is embedded", rec.Code)
	}
	// The API must keep working without a frontend.
	if apiRec := do(t, h, http.MethodGet, "/api/snapshot", ""); apiRec.Code != http.StatusOK {
		t.Errorf("api status = %d, want 200 with no static FS", apiRec.Code)
	}
}

func TestLooksLikeAsset(t *testing.T) {
	cases := map[string]bool{
		"assets/app-abc.js": true,
		"static/x.css":      true,
		"favicon.ico":       true,
		"a/b/c.png":         true,
		"scenarios":         false,
		"node/build":        false,
		"":                  false,
	}
	for name, want := range cases {
		if got := looksLikeAsset(name); got != want {
			t.Errorf("looksLikeAsset(%q) = %v, want %v", name, got, want)
		}
	}
}
