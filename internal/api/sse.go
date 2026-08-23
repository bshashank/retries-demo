package api

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// handleStream pushes the full Snapshot as a JSON SSE frame on a fixed tick.
//
// Design notes, all of which matter behind Cloud Run's proxy:
//   - X-Accel-Buffering: no stops nginx-family proxies from buffering the
//     stream into oblivion; without it the client sees nothing for minutes.
//   - Every frame is followed by an explicit Flush; Go buffers writes otherwise.
//   - The first snapshot goes out immediately so the UI paints on connect
//     instead of staring at an empty graph for a tick.
//   - A comment keepalive keeps idle-connection reapers from dropping the
//     stream when the simulation is quiet.
//   - The loop exits on r.Context().Done(), so a disconnecting client reaps its
//     goroutine and tickers instead of leaking them.
//   - Concurrent connections are capped at maxStreamConnections (a.streamSem);
//     past that, new connections get a 503 immediately rather than the
//     unbounded stream count starving Cloud Run's per-instance request budget.
//
// Each connection gets its own goroutine and its own tickers; the only shared
// state is Controller.Snapshot, which the contract documents as concurrency
// safe. Multiple viewers therefore watch one authoritative world.
func (a *API) handleStream(w http.ResponseWriter, r *http.Request) {
	// A nil streamSem (an *API built by hand rather than via NewRouter, as
	// some tests do to isolate one code path) means "no limit" rather than
	// "always full": sending on a nil channel never succeeds, so the naive
	// version of this check would 503 every request.
	if a.streamSem != nil {
		select {
		case a.streamSem <- struct{}{}:
			defer func() { <-a.streamSem }()
		default:
			writeError(w, http.StatusServiceUnavailable, "too many concurrent streams; try again shortly")
			return
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported by this server")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Content-Type-Options", "nosniff")
	if r.ProtoMajor == 1 {
		// Connection is a hop-by-hop header and illegal under HTTP/2, where
		// persistent streams are the default anyway.
		h.Set("Connection", "keep-alive")
	}

	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()

	sendSnapshot := func() bool {
		payload, err := json.Marshal(a.ctrl.Snapshot())
		if err != nil {
			a.log.Error("sse: marshal snapshot", "err", err)
			return false
		}
		// SSE frame: "data: <json>\n\n". Marshal never emits a raw newline, so
		// one data line always holds the whole object.
		if _, err := w.Write(append(append([]byte("data: "), payload...), '\n', '\n')); err != nil {
			return false // client hung up mid-write
		}
		flusher.Flush()
		return true
	}

	if !sendSnapshot() {
		return
	}

	ticker := time.NewTicker(a.snapshotInterval)
	defer ticker.Stop()
	keepalive := time.NewTicker(a.keepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !sendSnapshot() {
				return
			}
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
