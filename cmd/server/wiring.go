package main

import (
	"log/slog"

	"pipelinehealth/internal/sim"
)

// newSimController builds the simulation engine and returns it behind the
// sim.Controller interface plus a shutdown func.
//
// ============================== ADJUST ME ===================================
// The simulation package is being written in parallel. Once it exposes its
// engine, replace the placeholder block below with the single real line:
//
//	e := sim.New()               // or sim.NewEngine()
//	return e, func() { e.Close() } // or e.Stop()
//
// Nothing else in this command depends on the concrete type: main.go and the
// whole api package speak only sim.Controller.
// ============================================================================
func newSimController(logger *slog.Logger) (sim.Controller, func()) {
	logger.Info("starting real simulation engine")
	e := sim.New()
	return e, func() { e.Close() }
}
