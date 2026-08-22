package sim

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// Temporary verification harness. Run with:
//   SIM_TIMELINE=1 go test ./internal/sim/ -run TestPhysicsTimeline -v -timeout 15m
func TestPhysicsTimeline(t *testing.T) {
	if os.Getenv("SIM_TIMELINE") == "" {
		t.Skip("set SIM_TIMELINE=1")
	}

	e := New()
	defer e.Close()

	report := func(tag string, dur time.Duration) {
		fmt.Printf("\n=== %s (%v) ===\n", tag, dur)
		fmt.Printf("%6s %-9s %-6s %-6s %s\n", "t", "global", "runs/s", "ok%", "per-node: local/rollup q=depth wait p95 err rej ab")
		deadline := time.Now().Add(dur)
		start := time.Now()
		for time.Now().Before(deadline) {
			s := e.Snapshot()
			el := time.Since(start).Seconds()
			line := ""
			for _, n := range s.Nodes {
				line += fmt.Sprintf("\n         %-18s %-8s/%-8s q=%3d/%3d wait=%7.1f p95=%7.1f err=%.2f rej=%.2f ab=%.2f infl=%d",
					n.ID, n.LocalStatus, n.RollupStatus, n.QueueDepth, n.QueueCapacity,
					n.MeanQueueWaitMs, n.P95LatencyMs, n.ErrorRate, n.RejectRate, n.AbandonRate, n.InFlight)
			}
			fmt.Printf("%6.1f %-9s %6.1f %5.2f%s\n", el, s.Global, s.RunsPerSec, s.RunSuccessRate, line)
			time.Sleep(2 * time.Second)
		}
	}

	summary := func(tag string) {
		s := e.Snapshot()
		fmt.Printf(">>> SUMMARY %s: global=%s runs/s=%.1f ok=%.3f runP95=%.0fms\n", tag, s.Global, s.RunsPerSec, s.RunSuccessRate, s.RunP95Ms)
		for _, n := range s.Nodes {
			fmt.Printf("      %-18s %-8s/%-8s q=%3d wait=%7.1fms p50=%7.1f p95=%7.1f thr=%5.1f err=%.2f rej=%.2f ab=%.2f\n",
				n.ID, n.LocalStatus, n.RollupStatus, n.QueueDepth, n.MeanQueueWaitMs,
				n.P50LatencyMs, n.P95LatencyMs, n.ThroughputPerSec, n.ErrorRate, n.RejectRate, n.AbandonRate)
		}
	}

	report("BASELINE", 12*time.Second)
	summary("baseline")

	// --- sast-slow ---
	fmt.Println("\n### applying sast-slow")
	injectAt := time.Now()
	if err := e.ApplyScenario(ScenarioSASTSlow); err != nil {
		t.Fatal(err)
	}
	firstDeg := time.Duration(-1)
	sawFailing := false
	maxOrchQ, maxOrchWait := 0, 0.0
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		s := e.Snapshot()
		if s.Global == StatusDegraded && firstDeg < 0 {
			firstDeg = time.Since(injectAt)
		}
		if s.Global == StatusFailing {
			sawFailing = true
		}
		o := nodeByID(s, NodeOrchestrator)
		if o.QueueDepth > maxOrchQ {
			maxOrchQ = o.QueueDepth
		}
		if o.MeanQueueWaitMs > maxOrchWait {
			maxOrchWait = o.MeanQueueWaitMs
		}
		time.Sleep(200 * time.Millisecond)
	}
	summary("sast-slow")
	fmt.Printf(">>> sast-slow: firstDEGRADED=%v everFAILING=%v maxOrchQueue=%d maxOrchWait=%.1fms\n",
		firstDeg.Round(100*time.Millisecond), sawFailing, maxOrchQ, maxOrchWait)

	// --- recover ---
	fmt.Println("\n### reset")
	resetAt := time.Now()
	e.Reset()
	recovered := time.Duration(-1)
	for time.Now().Before(resetAt.Add(30 * time.Second)) {
		if e.Snapshot().Global == StatusOK {
			recovered = time.Since(resetAt)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf(">>> recovery to OK after reset: %v\n", recovered.Round(100*time.Millisecond))
	time.Sleep(4 * time.Second)

	// --- artifact-outage ---
	fmt.Println("\n### applying artifact-outage")
	injectAt = time.Now()
	if err := e.ApplyScenario(ScenarioArtifactOutage); err != nil {
		t.Fatal(err)
	}
	firstFail := time.Duration(-1)
	for time.Now().Before(injectAt.Add(40 * time.Second)) {
		if e.Snapshot().Global == StatusFailing {
			firstFail = time.Since(injectAt)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf(">>> artifact-outage: firstFAILING=%v\n", firstFail.Round(100*time.Millisecond))
	report("ARTIFACT-OUTAGE HOLD", 10*time.Second)
	summary("artifact-outage")

	fmt.Println("\n### reset")
	resetAt = time.Now()
	e.Reset()
	recovered = -1
	for time.Now().Before(resetAt.Add(40 * time.Second)) {
		if e.Snapshot().Global == StatusOK {
			recovered = time.Since(resetAt)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf(">>> recovery to OK after reset: %v\n", recovered.Round(100*time.Millisecond))
	time.Sleep(4 * time.Second)

	// --- kafka-lag ---
	fmt.Println("\n### applying kafka-lag")
	injectAt = time.Now()
	if err := e.ApplyScenario(ScenarioKafkaLag); err != nil {
		t.Fatal(err)
	}
	firstDeg = -1
	sawFailing = false
	maxKafkaQ := 0
	maxOrchQ, maxOrchWait = 0, 0.0
	for time.Now().Before(injectAt.Add(25 * time.Second)) {
		s := e.Snapshot()
		if s.Global == StatusDegraded && firstDeg < 0 {
			firstDeg = time.Since(injectAt)
		}
		if s.Global == StatusFailing {
			sawFailing = true
		}
		if k := nodeByID(s, NodeKafka); k.QueueDepth > maxKafkaQ {
			maxKafkaQ = k.QueueDepth
		}
		o := nodeByID(s, NodeOrchestrator)
		if o.QueueDepth > maxOrchQ {
			maxOrchQ = o.QueueDepth
		}
		if o.MeanQueueWaitMs > maxOrchWait {
			maxOrchWait = o.MeanQueueWaitMs
		}
		time.Sleep(200 * time.Millisecond)
	}
	summary("kafka-lag")
	fmt.Printf(">>> kafka-lag: firstDEGRADED=%v everFAILING=%v maxKafkaQueue=%d maxOrchQueue=%d maxOrchWait=%.1fms\n",
		firstDeg.Round(100*time.Millisecond), sawFailing, maxKafkaQ, maxOrchQ, maxOrchWait)
}
