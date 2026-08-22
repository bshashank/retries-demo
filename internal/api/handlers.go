package api

import (
	"fmt"
	"math"
	"net/http"

	"pipelinehealth/internal/sim"
)

// handleSnapshot returns one state snapshot. The SSE stream is the primary
// transport; this exists for curl, tests, and clients that cannot stream.
func (a *API) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.ctrl.Snapshot())
}

// handleScenarios lists the presets the UI offers.
func (a *API) handleScenarios(w http.ResponseWriter, r *http.Request) {
	list := a.ctrl.Scenarios()
	if list == nil {
		// Marshal an empty array rather than null: the UI maps over this.
		list = []sim.ScenarioInfo{}
	}
	writeJSON(w, http.StatusOK, list)
}

type injectRequest struct {
	NodeID string `json:"nodeId"`
	// Pointers so an omitted field is distinguishable from an explicit zero and
	// reported as "required" instead of "out of range".
	LatencyMultiplier *float64 `json:"latencyMultiplier"`
	FailRate          *float64 `json:"failRate"`
}

func (a *API) handleInject(w http.ResponseWriter, r *http.Request) {
	var req injectRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.NodeID == "" {
		writeError(w, http.StatusBadRequest, "nodeId is required")
		return
	}
	if req.LatencyMultiplier == nil {
		writeError(w, http.StatusBadRequest, "latencyMultiplier is required")
		return
	}
	if req.FailRate == nil {
		writeError(w, http.StatusBadRequest, "failRate is required")
		return
	}
	if err := checkRange("latencyMultiplier", *req.LatencyMultiplier, minLatencyMultiplier, maxLatencyMultiplier); err != nil {
		writeAPIError(w, err)
		return
	}
	if err := checkRange("failRate", *req.FailRate, minFailRate, maxFailRate); err != nil {
		writeAPIError(w, err)
		return
	}

	if err := a.ctrl.Inject(req.NodeID, *req.LatencyMultiplier, *req.FailRate); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, okBody{OK: true})
}

type edgeRequest struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Essential bool   `json:"essential"`
}

func (a *API) handleEdge(w http.ResponseWriter, r *http.Request) {
	var req edgeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.From == "" {
		writeError(w, http.StatusBadRequest, "from is required")
		return
	}
	if req.To == "" {
		writeError(w, http.StatusBadRequest, "to is required")
		return
	}

	if err := a.ctrl.SetEdgeEssential(req.From, req.To, req.Essential); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, okBody{OK: true})
}

type scenarioRequest struct {
	Name string `json:"name"`
}

func (a *API) handleScenario(w http.ResponseWriter, r *http.Request) {
	var req scenarioRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	if err := a.ctrl.ApplyScenario(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, okBody{OK: true})
}

func (a *API) handleReset(w http.ResponseWriter, r *http.Request) {
	drainBody(w, r)
	a.ctrl.Reset()
	writeJSON(w, http.StatusOK, okBody{OK: true})
}

// handleHealth is the Cloud Run liveness probe. It deliberately does not touch
// the simulation: this answers "is the process serving?", not "is the world
// healthy?" — a DEGRADED pipeline is the demo working, not the server failing.
func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// checkRange rejects non-finite and out-of-bounds values, naming the field.
func checkRange(field string, v, lo, hi float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return badRequest(fmt.Sprintf("%s must be a finite number", field))
	}
	if v < lo || v > hi {
		return badRequest(fmt.Sprintf("%s must be between %g and %g (got %g)", field, lo, hi, v))
	}
	return nil
}
