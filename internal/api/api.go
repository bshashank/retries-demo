// Package api is the HTTP layer for the pipeline-health demo.
//
// It depends only on the sim.Controller interface, never on a concrete engine
// type, so handlers are testable against a hand-written fake and the package
// compiles independently of the simulation core.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"pipelinehealth/internal/sim"
)

const (
	// maxBodyBytes caps every request body. The control endpoints take a few
	// dozen bytes of JSON; anything larger is a client bug or an attack.
	maxBodyBytes = 8 << 10 // 8 KiB

	defaultSnapshotInterval  = 200 * time.Millisecond
	defaultKeepaliveInterval = 15 * time.Second

	// Accepted ranges for injected fault parameters. Out-of-range values are
	// rejected with a 400 naming the field rather than silently clamped, so an
	// operator never believes a fault was applied when it was not.
	minLatencyMultiplier = 0.1
	maxLatencyMultiplier = 100
	minFailRate          = 0
	maxFailRate          = 1

	// maxStreamConnections caps concurrent /api/stream clients. Cloud Run's
	// default per-instance concurrency is 80, and this deploy deliberately
	// pins --max-instances 1 (see DEPLOY.md) — with no cap here, a script
	// opening ~80 unauthenticated SSE connections and holding them open (the
	// stream has no server-side lifetime limit by design) would alone
	// exhaust every request slot the single instance has, taking down "/",
	// "/api/snapshot", and everyone else's stream too, not just degrading
	// the shared simulation state the way the other endpoints intentionally
	// can. Reserving headroom below 80 means an attempted exhaustion gets a
	// clean 503 past this limit instead of starving the whole service.
	maxStreamConnections = 40
)

// Options configures the router.
type Options struct {
	// Dev enables permissive CORS so the Vite dev server (localhost:5173) can
	// call the API cross-origin. Must stay false in production.
	Dev bool

	// Static is the built frontend to serve at "/" with SPA fallback. When nil,
	// non-API routes return a plain 404.
	Static fs.FS

	// SnapshotInterval is the SSE push period. Zero means 200ms.
	SnapshotInterval time.Duration

	// KeepaliveInterval is the SSE comment-frame period that keeps idle
	// connections alive through proxies. Zero means 15s.
	KeepaliveInterval time.Duration

	// Logger receives operational messages. Zero means slog.Default().
	Logger *slog.Logger
}

// API holds the handler dependencies.
type API struct {
	ctrl              sim.Controller
	snapshotInterval  time.Duration
	keepaliveInterval time.Duration
	log               *slog.Logger

	// streamSem bounds concurrent SSE connections. A buffered channel used as
	// a counting semaphore: acquire by sending, release by receiving.
	streamSem chan struct{}
}

// NewRouter builds the complete HTTP handler: the JSON/SSE API, the liveness
// probe, and (when Options.Static is set) the embedded SPA.
func NewRouter(ctrl sim.Controller, opts Options) http.Handler {
	a := &API{
		ctrl:              ctrl,
		snapshotInterval:  opts.SnapshotInterval,
		keepaliveInterval: opts.KeepaliveInterval,
		log:               opts.Logger,
		streamSem:         make(chan struct{}, maxStreamConnections),
	}
	if a.snapshotInterval <= 0 {
		a.snapshotInterval = defaultSnapshotInterval
	}
	if a.keepaliveInterval <= 0 {
		a.keepaliveInterval = defaultKeepaliveInterval
	}
	if a.log == nil {
		a.log = slog.Default()
	}

	mux := http.NewServeMux()

	mux.Handle("/api/stream", allow(a.handleStream, http.MethodGet))
	mux.Handle("/api/snapshot", allow(a.handleSnapshot, http.MethodGet, http.MethodHead))
	mux.Handle("/api/scenarios", allow(a.handleScenarios, http.MethodGet, http.MethodHead))
	mux.Handle("/api/inject", allow(a.handleInject, http.MethodPost))
	mux.Handle("/api/edge", allow(a.handleEdge, http.MethodPost))
	mux.Handle("/api/scenario", allow(a.handleScenario, http.MethodPost))
	mux.Handle("/api/reset", allow(a.handleReset, http.MethodPost))

	// Unknown /api/* paths answer with JSON, never with the SPA's HTML: an XHR
	// that gets a 200 text/html back is far harder to debug than a 404 body.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint: "+r.URL.Path)
	})

	mux.Handle("/healthz", allow(a.handleHealth, http.MethodGet, http.MethodHead))

	if opts.Static != nil {
		mux.Handle("/", spaHandler(opts.Static))
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
	}

	var h http.Handler = mux
	if opts.Dev {
		h = withDevCORS(h)
	}
	return h
}

// allow wraps a handler with a method guard that answers 405 (plus an Allow
// header) in JSON, keeping every error body on this API the same shape.
func allow(h http.HandlerFunc, methods ...string) http.Handler {
	permitted := make(map[string]bool, len(methods))
	for _, m := range methods {
		permitted[m] = true
	}
	allowHeader := strings.Join(methods, ", ")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !permitted[r.Method] {
			w.Header().Set("Allow", allowHeader)
			writeError(w, http.StatusMethodNotAllowed,
				fmt.Sprintf("method %s not allowed on %s (allowed: %s)", r.Method, r.URL.Path, allowHeader))
			return
		}
		h(w, r)
	})
}

// withDevCORS allows any origin. Enabled only by -dev / DEV=1 so that the
// production deployment, which serves the UI from the same origin, stays shut.
func withDevCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Content-Type")
			h.Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- responses --

type errorBody struct {
	Error string `json:"error"`
}

type okBody struct {
	OK bool `json:"ok"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// Encoding our own response types cannot fail in practice; if it ever
		// does, do not leave the client hanging on a header-less connection.
		http.Error(w, `{"error":"internal encoding failure"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError takes a plain message (not a format string) so that controller
// error text containing '%' can never be interpreted as a verb.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

// ------------------------------------------------------------------ decoding --

// apiError carries the status code a decode/validation failure should produce.
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(msg string) error { return &apiError{status: http.StatusBadRequest, msg: msg} }

// writeAPIError renders an apiError, defaulting to 400 for anything else.
func writeAPIError(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeError(w, ae.status, ae.msg)
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

// decodeJSON reads a size-capped, strictly-validated JSON body into dst.
// Unknown fields, trailing garbage, and oversized bodies are all rejected.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError

		switch {
		case errors.As(err, &maxErr):
			return &apiError{
				status: http.StatusRequestEntityTooLarge,
				msg:    fmt.Sprintf("request body too large (limit %d bytes)", maxBodyBytes),
			}
		case errors.Is(err, io.EOF):
			return badRequest("request body is empty; expected a JSON object")
		case errors.As(err, &syntaxErr):
			return badRequest(fmt.Sprintf("malformed JSON at byte offset %d", syntaxErr.Offset))
		case errors.As(err, &typeErr):
			return badRequest(fmt.Sprintf("field %q must be a JSON %s", typeErr.Field, typeErr.Type))
		case errors.Is(err, io.ErrUnexpectedEOF):
			return badRequest("malformed JSON: unexpected end of body")
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			return badRequest("unknown field " + strings.TrimPrefix(err.Error(), "json: unknown field "))
		default:
			return badRequest("malformed JSON: " + err.Error())
		}
	}

	if dec.More() {
		return badRequest("unexpected data after the JSON object; send exactly one")
	}
	return nil
}

// drainBody enforces the size cap on endpoints that take no body, so a client
// cannot stream an unbounded payload at /api/reset.
func drainBody(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	_, _ = io.Copy(io.Discard, r.Body)
}
