package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"pipelinehealth/internal/sim"
)

// streamServer starts a real HTTP server (httptest.NewRecorder cannot stream)
// and reports when the stream handler goroutine returns, which is how the
// disconnect test proves the handler is not leaked.
func streamServer(t *testing.T, f *fakeController, opts Options) (*httptest.Server, <-chan struct{}) {
	t.Helper()

	router := NewRouter(f, opts)
	handlerReturned := make(chan struct{}, 8)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/stream" {
			defer func() {
				select {
				case handlerReturned <- struct{}{}:
				default:
				}
			}()
		}
		router.ServeHTTP(w, r)
	}))
	return srv, handlerReturned
}

// readFrame reads one SSE frame: lines up to (and including) the blank
// separator line. It is bounded by the caller's context deadline via the
// underlying connection, and by readFrameWithin below.
func readFrame(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return sb.String(), err
		}
		if line == "\n" || line == "\r\n" {
			return sb.String(), nil
		}
		sb.WriteString(line)
	}
}

// readFrameWithin fails the test rather than hanging the whole suite when a
// regression stops the stream from producing frames.
func readFrameWithin(t *testing.T, br *bufio.Reader, d time.Duration) string {
	t.Helper()
	type result struct {
		frame string
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		frame, err := readFrame(br)
		ch <- result{frame, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("reading SSE frame: %v (partial=%q)", r.err, r.frame)
		}
		return r.frame
	case <-time.After(d):
		t.Fatalf("no SSE frame within %s", d)
		return ""
	}
}

func TestSSEHeadersAndFrameFormat(t *testing.T) {
	f := newFake()
	srv, _ := streamServer(t, f, Options{
		SnapshotInterval:  20 * time.Millisecond,
		KeepaliveInterval: time.Hour,
	})
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Cloud Run / nginx-family proxies buffer or drop the stream without these.
	wantHeaders := map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"Connection":        "keep-alive",
		"X-Accel-Buffering": "no",
	}
	for k, want := range wantHeaders {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}

	br := bufio.NewReader(resp.Body)
	frame := readFrameWithin(t, br, 3*time.Second)

	if !strings.HasPrefix(frame, "data: ") {
		t.Fatalf("frame = %q, want it to start with %q", frame, "data: ")
	}
	if !strings.HasSuffix(frame, "\n") {
		t.Errorf("frame %q is not newline-terminated", frame)
	}

	payload := strings.TrimSuffix(strings.TrimPrefix(frame, "data: "), "\n")
	var got sim.Snapshot
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("frame payload is not a sim.Snapshot: %v (payload=%q)", err, payload)
	}
	if got.Global != sim.StatusDegraded {
		t.Errorf("streamed global = %q, want DEGRADED", got.Global)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].ID != sim.NodeBuild {
		t.Errorf("streamed nodes = %+v, want the fake's build node", got.Nodes)
	}
}

// The first snapshot must not wait for the first tick, or the UI shows an empty
// graph on connect.
func TestSSESendsInitialSnapshotImmediately(t *testing.T) {
	f := newFake()
	srv, _ := streamServer(t, f, Options{
		SnapshotInterval:  10 * time.Second, // far longer than the assertion window
		KeepaliveInterval: time.Hour,
	})
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	start := time.Now()
	frame := readFrameWithin(t, bufio.NewReader(resp.Body), 2*time.Second)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("first frame took %s; it must not wait for the tick", elapsed)
	}
	if !strings.HasPrefix(frame, "data: ") {
		t.Errorf("first frame = %q, want a data frame", frame)
	}
}

func TestSSEPushesRepeatedly(t *testing.T) {
	f := newFake()
	srv, _ := streamServer(t, f, Options{
		SnapshotInterval:  15 * time.Millisecond,
		KeepaliveInterval: time.Hour,
	})
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	for i := range 3 {
		frame := readFrameWithin(t, br, 3*time.Second)
		if !strings.HasPrefix(frame, "data: ") {
			t.Fatalf("frame %d = %q, want a data frame", i, frame)
		}
	}
}

func TestSSEEmitsKeepaliveComment(t *testing.T) {
	f := newFake()
	srv, _ := streamServer(t, f, Options{
		SnapshotInterval:  10 * time.Second, // keep data frames out of the way
		KeepaliveInterval: 20 * time.Millisecond,
	})
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	readFrameWithin(t, br, 3*time.Second) // the immediate snapshot

	frame := readFrameWithin(t, br, 3*time.Second)
	if !strings.HasPrefix(frame, ":") {
		t.Fatalf("frame = %q, want an SSE comment keepalive", frame)
	}
}

// A disconnecting client must terminate the handler, or every reconnect leaks a
// goroutine and a ticker for the life of the process.
func TestSSEClientDisconnectTerminatesHandler(t *testing.T) {
	f := newFake()
	srv, handlerReturned := streamServer(t, f, Options{
		SnapshotInterval:  15 * time.Millisecond,
		KeepaliveInterval: time.Hour,
	})
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("connect: %v", err)
	}

	// Confirm the stream is live before cutting it.
	readFrameWithin(t, bufio.NewReader(resp.Body), 3*time.Second)

	cancel()
	resp.Body.Close()

	select {
	case <-handlerReturned:
		// Handler observed r.Context().Done() and unwound.
	case <-time.After(5 * time.Second):
		t.Fatal("stream handler did not return within 5s of client disconnect: goroutine leak")
	}
}

func TestSSEMultipleConcurrentClients(t *testing.T) {
	const clients = 8

	f := newFake()
	srv, _ := streamServer(t, f, Options{
		SnapshotInterval:  10 * time.Millisecond,
		KeepaliveInterval: time.Hour,
	})
	defer srv.Close()

	var wg sync.WaitGroup
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/stream", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("client %d connect: %v", i, err)
				return
			}
			defer resp.Body.Close()

			br := bufio.NewReader(resp.Body)
			for range 2 {
				frame, err := readFrame(br)
				if err != nil {
					t.Errorf("client %d read: %v", i, err)
					return
				}
				if !strings.HasPrefix(frame, "data: ") {
					t.Errorf("client %d frame = %q, want a data frame", i, frame)
					return
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("concurrent SSE clients did not all receive frames")
	}
}

// TestSSEConnectionCapReturns503 is the fix for a real pre-launch finding: an
// unbounded number of concurrent /api/stream connections could alone exhaust
// Cloud Run's per-instance request-concurrency budget (this deploy pins
// --max-instances 1), taking the whole service down for every route, not
// just degrading the shared simulation state the other endpoints
// intentionally allow. Once maxStreamConnections is held open, the next
// connection must get a clean 503 rather than either blocking forever or
// being admitted anyway; freeing a slot must let a new connection back in.
func TestSSEConnectionCapReturns503(t *testing.T) {
	f := newFake()
	srv, _ := streamServer(t, f, Options{
		SnapshotInterval:  time.Hour,
		KeepaliveInterval: time.Hour,
	})
	defer srv.Close()

	connect := func() (*http.Response, context.CancelFunc, error) {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/stream", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			return nil, nil, err
		}
		return resp, cancel, nil
	}

	// Fill every slot and confirm each one actually connected (200, not 503).
	type held struct {
		resp   *http.Response
		cancel context.CancelFunc
	}
	conns := make([]held, 0, maxStreamConnections)
	defer func() {
		for _, c := range conns {
			c.cancel()
			c.resp.Body.Close()
		}
	}()

	for i := 0; i < maxStreamConnections; i++ {
		resp, cancel, err := connect()
		if err != nil {
			t.Fatalf("client %d: connect: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("client %d: status = %d, want 200 (slot %d of %d should still be free)",
				i, resp.StatusCode, i, maxStreamConnections)
		}
		conns = append(conns, held{resp: resp, cancel: cancel})
	}

	// One more, past the cap, must be refused rather than admitted or hung.
	over, overCancel, err := connect()
	if err != nil {
		t.Fatalf("connect past the cap: %v", err)
	}
	defer overCancel()
	defer over.Body.Close()
	if over.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status past the cap = %d, want 503", over.StatusCode)
	}

	// Freeing one slot must let a new connection through.
	conns[0].cancel()
	conns[0].resp.Body.Close()
	conns = conns[1:]

	deadline := time.Now().Add(3 * time.Second)
	var reconnected *http.Response
	var reconnectCancel context.CancelFunc
	for time.Now().Before(deadline) {
		resp, cancel, err := connect()
		if err == nil && resp.StatusCode == http.StatusOK {
			reconnected, reconnectCancel = resp, cancel
			break
		}
		if err == nil {
			resp.Body.Close()
			cancel()
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reconnected == nil {
		t.Fatal("freeing a slot did not let a new connection through within 3s")
	}
	reconnectCancel()
	reconnected.Body.Close()
}

// nonFlushWriter deliberately does not implement http.Flusher.
type nonFlushWriter struct{ rec *httptest.ResponseRecorder }

func (w *nonFlushWriter) Header() http.Header         { return w.rec.Header() }
func (w *nonFlushWriter) Write(b []byte) (int, error) { return w.rec.Write(b) }
func (w *nonFlushWriter) WriteHeader(code int)        { w.rec.WriteHeader(code) }

func TestSSEWithoutFlusherFailsFast(t *testing.T) {
	a := &API{
		ctrl:              newFake(),
		snapshotInterval:  10 * time.Millisecond,
		keepaliveInterval: time.Hour,
		log:               discardLogger(),
	}

	rec := httptest.NewRecorder()
	w := &nonFlushWriter{rec: rec}
	req := httptest.NewRequest(http.MethodGet, "/api/stream", nil)

	done := make(chan struct{})
	go func() {
		a.handleStream(w, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handleStream blocked on a non-flushing writer; it must 500 immediately")
	}

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
