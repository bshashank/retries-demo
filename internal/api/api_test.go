package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"pipelinehealth/internal/sim"
)

// ------------------------------------------------------------------- fake ---

// fakeController is a hand-written sim.Controller. The api package must never
// depend on the concrete engine, and this test proves it: the suite compiles and
// passes with no simulation implementation present at all.
type fakeController struct {
	mu sync.Mutex

	snapshot  sim.Snapshot
	scenarios []sim.ScenarioInfo

	// Canned errors, to exercise error propagation as 400.
	injectErr   error
	edgeErr     error
	scenarioErr error

	// Recorded calls.
	injectCalls   []injectCall
	edgeCalls     []edgeCall
	scenarioCalls []string
	resetCalls    int
}

type injectCall struct {
	nodeID   string
	latency  float64
	failRate float64
}

type edgeCall struct {
	from, to  string
	essential bool
}

func newFake() *fakeController {
	return &fakeController{
		snapshot: sim.Snapshot{
			AtMs:           1700000000000,
			Global:         sim.StatusDegraded,
			RunsPerSec:     12.5,
			RunSuccessRate: 0.91,
			RunP95Ms:       842,
			Nodes: []sim.NodeSnapshot{{
				ID: sim.NodeBuild, Label: "Build", Tier: 1,
				LocalStatus: sim.StatusOK, RollupStatus: sim.StatusDegraded,
				QueueDepth: 7, QueueCapacity: 64, InFlight: 3, Workers: 4,
				LatencyMultiplier: 1,
			}},
			Edges: []sim.EdgeSnapshot{{
				From: sim.NodeOrchestrator, To: sim.NodeBuild, Essential: true, TimeoutMs: 2000,
			}},
			Events: []sim.Event{{
				ID: 42, AtMs: 1700000000000, Level: sim.LevelWarn, Message: "build queue filling",
			}},
		},
		scenarios: []sim.ScenarioInfo{{
			Name: "retry-storm", Label: "Retry Storm",
			Description: "Clients retry aggressively into a slow dependency.",
			Expected:    "Queue saturation cascades upstream.",
		}},
	}
}

func (f *fakeController) Snapshot() sim.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot
}

func (f *fakeController) Inject(nodeID string, latencyMultiplier, failRate float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.injectCalls = append(f.injectCalls, injectCall{nodeID, latencyMultiplier, failRate})
	return f.injectErr
}

func (f *fakeController) SetEdgeEssential(from, to string, essential bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edgeCalls = append(f.edgeCalls, edgeCall{from, to, essential})
	return f.edgeErr
}

func (f *fakeController) ApplyScenario(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scenarioCalls = append(f.scenarioCalls, name)
	return f.scenarioErr
}

func (f *fakeController) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetCalls++
}

func (f *fakeController) Scenarios() []sim.ScenarioInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scenarios
}

// Compile-time proof the fake matches the frozen contract.
var _ sim.Controller = (*fakeController)(nil)

// ------------------------------------------------------------------ helpers --

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":         &fstest.MapFile{Data: []byte("<!doctype html><title>shell</title>")},
		"assets/app-abc.js":  &fstest.MapFile{Data: []byte("console.log('hi')")},
		"assets/app-abc.css": &fstest.MapFile{Data: []byte("body{}")},
	}
}

func newTestRouter(t *testing.T, f *fakeController) http.Handler {
	t.Helper()
	return NewRouter(f, Options{Static: testFS()})
}

func do(t *testing.T, h http.Handler, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error response is not JSON: %v (body=%q)", err, rec.Body.String())
	}
	if body.Error == "" {
		t.Fatalf("error response has an empty error field: %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("error Content-Type = %q, want application/json", ct)
	}
	return body.Error
}

// -------------------------------------------------------------- happy paths --

func TestHealthz(t *testing.T) {
	rec := do(t, newTestRouter(t, newFake()), http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %q, want ok", body["status"])
	}
}

func TestSnapshotEndpoint(t *testing.T) {
	f := newFake()
	rec := do(t, newTestRouter(t, f), http.MethodGet, "/api/snapshot", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got sim.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if got.Global != sim.StatusDegraded {
		t.Errorf("global = %q, want DEGRADED", got.Global)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].ID != sim.NodeBuild {
		t.Errorf("nodes = %+v, want one build node", got.Nodes)
	}
	if len(got.Edges) != 1 || len(got.Events) != 1 {
		t.Errorf("edges/events not round-tripped: %+v / %+v", got.Edges, got.Events)
	}

	// Wire-format check: the frozen JSON tags must survive the handler.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	for _, key := range []string{"atMs", "global", "runsPerSec", "runSuccessRate", "runP95Ms", "nodes", "edges", "events"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("snapshot JSON missing key %q", key)
		}
	}
}

func TestScenariosEndpoint(t *testing.T) {
	f := newFake()
	rec := do(t, newTestRouter(t, f), http.MethodGet, "/api/scenarios", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []sim.ScenarioInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Name != "retry-storm" {
		t.Fatalf("scenarios = %+v, want retry-storm", got)
	}
	if got[0].Label == "" || got[0].Description == "" || got[0].Expected == "" {
		t.Errorf("scenario fields dropped: %+v", got[0])
	}
}

func TestScenariosEmptyMarshalsAsArray(t *testing.T) {
	f := newFake()
	f.scenarios = nil
	rec := do(t, newTestRouter(t, f), http.MethodGet, "/api/scenarios", "")
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %q, want [] (never null: the UI maps over this)", got)
	}
}

func TestInjectHappyPath(t *testing.T) {
	f := newFake()
	rec := do(t, newTestRouter(t, f), http.MethodPost, "/api/inject",
		`{"nodeId":"build","latencyMultiplier":4.5,"failRate":0.25}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body okBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !body.OK {
		t.Fatalf("body = %q, want {\"ok\":true} (err=%v)", rec.Body.String(), err)
	}
	if len(f.injectCalls) != 1 {
		t.Fatalf("inject called %d times, want 1", len(f.injectCalls))
	}
	if got := f.injectCalls[0]; got.nodeID != "build" || got.latency != 4.5 || got.failRate != 0.25 {
		t.Errorf("inject args = %+v, want {build 4.5 0.25}", got)
	}
}

func TestInjectBoundaryValuesAccepted(t *testing.T) {
	for _, body := range []string{
		`{"nodeId":"build","latencyMultiplier":0.1,"failRate":0}`,
		`{"nodeId":"build","latencyMultiplier":100,"failRate":1}`,
	} {
		f := newFake()
		rec := do(t, newTestRouter(t, f), http.MethodPost, "/api/inject", body)
		if rec.Code != http.StatusOK {
			t.Errorf("body %s: status = %d, want 200 (%s)", body, rec.Code, rec.Body.String())
		}
	}
}

func TestEdgeHappyPath(t *testing.T) {
	f := newFake()
	rec := do(t, newTestRouter(t, f), http.MethodPost, "/api/edge",
		`{"from":"orchestrator","to":"telemetry","essential":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(f.edgeCalls) != 1 {
		t.Fatalf("SetEdgeEssential called %d times, want 1", len(f.edgeCalls))
	}
	want := edgeCall{"orchestrator", "telemetry", false}
	if f.edgeCalls[0] != want {
		t.Errorf("edge args = %+v, want %+v", f.edgeCalls[0], want)
	}
}

func TestScenarioHappyPath(t *testing.T) {
	f := newFake()
	rec := do(t, newTestRouter(t, f), http.MethodPost, "/api/scenario", `{"name":"retry-storm"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(f.scenarioCalls) != 1 || f.scenarioCalls[0] != "retry-storm" {
		t.Errorf("scenario calls = %v, want [retry-storm]", f.scenarioCalls)
	}
}

func TestResetHappyPath(t *testing.T) {
	f := newFake()
	rec := do(t, newTestRouter(t, f), http.MethodPost, "/api/reset", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if f.resetCalls != 1 {
		t.Errorf("Reset called %d times, want 1", f.resetCalls)
	}
}

// -------------------------------------------------------------- validation --

func TestMalformedJSONRejected(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
	}{
		{"truncated object", "/api/inject", `{"nodeId":"build"`},
		{"not json at all", "/api/inject", `nope`},
		{"empty body", "/api/inject", ``},
		{"wrong type", "/api/inject", `{"nodeId":123,"latencyMultiplier":1,"failRate":0}`},
		{"unknown field", "/api/inject", `{"nodeId":"build","latencyMultiplier":1,"failRate":0,"bogus":true}`},
		{"trailing garbage", "/api/inject", `{"nodeId":"build","latencyMultiplier":1,"failRate":0} {}`},
		{"edge malformed", "/api/edge", `{`},
		{"edge unknown field", "/api/edge", `{"from":"a","to":"b","essential":true,"weight":3}`},
		{"scenario malformed", "/api/scenario", `["retry-storm"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, newTestRouter(t, newFake()), http.MethodPost, tc.path, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			decodeErrorBody(t, rec)
		})
	}
}

func TestMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		body      string
		wantField string
	}{
		{"no nodeId", "/api/inject", `{"latencyMultiplier":1,"failRate":0}`, "nodeId"},
		{"no latencyMultiplier", "/api/inject", `{"nodeId":"build","failRate":0}`, "latencyMultiplier"},
		{"no failRate", "/api/inject", `{"nodeId":"build","latencyMultiplier":1}`, "failRate"},
		{"no from", "/api/edge", `{"to":"build","essential":true}`, "from"},
		{"no to", "/api/edge", `{"from":"build","essential":true}`, "to"},
		{"no name", "/api/scenario", `{}`, "name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, newTestRouter(t, newFake()), http.MethodPost, tc.path, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if msg := decodeErrorBody(t, rec); !strings.Contains(msg, tc.wantField) {
				t.Errorf("error %q does not name the offending field %q", msg, tc.wantField)
			}
		})
	}
}

func TestOutOfRangeRejectedNamingTheField(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantField string
	}{
		{"latency too low", `{"nodeId":"build","latencyMultiplier":0.001,"failRate":0}`, "latencyMultiplier"},
		{"latency too high", `{"nodeId":"build","latencyMultiplier":1000,"failRate":0}`, "latencyMultiplier"},
		{"latency negative", `{"nodeId":"build","latencyMultiplier":-2,"failRate":0}`, "latencyMultiplier"},
		{"failRate above one", `{"nodeId":"build","latencyMultiplier":1,"failRate":1.5}`, "failRate"},
		{"failRate negative", `{"nodeId":"build","latencyMultiplier":1,"failRate":-0.1}`, "failRate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			rec := do(t, newTestRouter(t, f), http.MethodPost, "/api/inject", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			if msg := decodeErrorBody(t, rec); !strings.Contains(msg, tc.wantField) {
				t.Errorf("error %q does not name the offending field %q", msg, tc.wantField)
			}
			// Rejected, not silently clamped: the controller must not be called.
			if len(f.injectCalls) != 0 {
				t.Errorf("controller was called with an out-of-range value: %+v", f.injectCalls)
			}
		})
	}
}

func TestControllerErrorsBecome400(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		body    string
		arm     func(*fakeController)
		wantMsg string
	}{
		{
			"unknown node", "/api/inject", `{"nodeId":"nope","latencyMultiplier":2,"failRate":0}`,
			func(f *fakeController) { f.injectErr = errors.New("unknown node: nope") }, "unknown node",
		},
		{
			"unknown edge", "/api/edge", `{"from":"a","to":"b","essential":true}`,
			func(f *fakeController) { f.edgeErr = errors.New("unknown edge: a -> b") }, "unknown edge",
		},
		{
			"unknown scenario", "/api/scenario", `{"name":"nope"}`,
			func(f *fakeController) { f.scenarioErr = errors.New("unknown scenario: nope") }, "unknown scenario",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			tc.arm(f)
			rec := do(t, newTestRouter(t, f), http.MethodPost, tc.path, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if msg := decodeErrorBody(t, rec); !strings.Contains(msg, tc.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", msg, tc.wantMsg)
			}
		})
	}
}

// A controller error containing a percent sign must not be treated as a format
// string — a regression here would leak %!v(MISSING) into the UI.
func TestControllerErrorWithPercentIsNotFormatted(t *testing.T) {
	f := newFake()
	f.scenarioErr = errors.New("unknown scenario: 100%s of nothing")
	rec := do(t, newTestRouter(t, f), http.MethodPost, "/api/scenario", `{"name":"x"}`)
	if msg := decodeErrorBody(t, rec); !strings.Contains(msg, "100%s of nothing") {
		t.Errorf("error = %q, want the raw controller text preserved", msg)
	}
}

func TestWrongMethodReturns405(t *testing.T) {
	cases := []struct {
		method, path string
	}{
		{http.MethodPost, "/api/snapshot"},
		{http.MethodPost, "/api/scenarios"},
		{http.MethodPost, "/api/stream"},
		{http.MethodDelete, "/api/stream"},
		{http.MethodGet, "/api/inject"},
		{http.MethodGet, "/api/edge"},
		{http.MethodGet, "/api/scenario"},
		{http.MethodGet, "/api/reset"},
		{http.MethodPut, "/api/reset"},
		{http.MethodPost, "/healthz"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := do(t, newTestRouter(t, newFake()), tc.method, tc.path, "")
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405", rec.Code)
			}
			if allow := rec.Header().Get("Allow"); allow == "" {
				t.Error("405 response is missing an Allow header")
			}
			decodeErrorBody(t, rec)
		})
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	// Well past maxBodyBytes, and valid JSON, so only the size cap can reject it.
	huge := `{"nodeId":"` + strings.Repeat("x", maxBodyBytes*2) + `","latencyMultiplier":1,"failRate":0}`

	for _, path := range []string{"/api/inject", "/api/edge", "/api/scenario"} {
		t.Run(path, func(t *testing.T) {
			f := newFake()
			rec := do(t, newTestRouter(t, f), http.MethodPost, path, huge)
			if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 413 (or 400) for an oversized body", rec.Code)
			}
			decodeErrorBody(t, rec)
			if len(f.injectCalls) != 0 || len(f.edgeCalls) != 0 || len(f.scenarioCalls) != 0 {
				t.Error("oversized body reached the controller")
			}
		})
	}
}

func TestUnknownAPIPathReturnsJSON404(t *testing.T) {
	for _, path := range []string{"/api/nope", "/api/inject/extra", "/api/"} {
		t.Run(path, func(t *testing.T) {
			rec := do(t, newTestRouter(t, newFake()), http.MethodGet, path, "")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
			}
			decodeErrorBody(t, rec)
			if strings.Contains(rec.Body.String(), "<!doctype") {
				t.Error("unknown API path served the SPA shell instead of JSON")
			}
		})
	}
}

// --------------------------------------------------------------------- CORS --

func TestCORSOffByDefault(t *testing.T) {
	h := NewRouter(newFake(), Options{})
	req := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("CORS header present without -dev: %q", got)
	}
}

func TestCORSEnabledInDev(t *testing.T) {
	h := NewRouter(newFake(), Options{Dev: true})

	req := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("Allow-Origin = %q, want the Vite dev origin", got)
	}

	// Preflight.
	pre := httptest.NewRequest(http.MethodOptions, "/api/inject", nil)
	pre.Header.Set("Origin", "http://localhost:5173")
	pre.Header.Set("Access-Control-Request-Method", "POST")
	preRec := httptest.NewRecorder()
	h.ServeHTTP(preRec, pre)
	if preRec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", preRec.Code)
	}
	if !strings.Contains(preRec.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Errorf("preflight does not allow POST: %q", preRec.Header().Get("Access-Control-Allow-Methods"))
	}
}

// Guard against a body being echoed by writeJSON with the wrong content type.
func TestJSONResponsesSetNosniff(t *testing.T) {
	rec := do(t, newTestRouter(t, newFake()), http.MethodGet, "/api/snapshot", "")
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}
