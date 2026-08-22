package sim

// Local status thresholds. These are read off a node's own rolling window only.
const (
	failQueueWaitMs = 1000.0
	failRejectRate  = 0.20
	failErrorRate   = 0.50
	failAbandonRate = 0.50

	degradedQueueWaitMs = 250.0
	degradedRejectRate  = 0.01
	degradedErrorRate   = 0.10
	degradedAbandonRate = 0.10
)

// localStatus derives a node's health from its own saturation and errors only.
//
// Deliberately it never looks at a child's status (that is the rollup's job) and
// never looks at the injected multiplier. Reading the injection directly would
// make the UI flip the instant an operator clicks, which is a flag, not a
// symptom. Instead the injection slows the node, the queue backs up, and the
// status follows the queue: the delay between cause and symptom is the point.
func localStatus(v metricsView) Status {
	// Too little traffic to conclude anything. One unlucky sample should not
	// paint a node red.
	if v.TotalAttempts < minAttempts {
		return StatusOK
	}
	switch {
	case v.MeanQueueWaitMs > failQueueWaitMs ||
		v.RejectRate > failRejectRate ||
		v.ErrorRate > failErrorRate ||
		v.AbandonRate > failAbandonRate:
		return StatusFailing

	case v.MeanQueueWaitMs > degradedQueueWaitMs ||
		v.RejectRate > degradedRejectRate ||
		v.ErrorRate > degradedErrorRate ||
		v.AbandonRate > degradedAbandonRate:
		return StatusDegraded
	}
	return StatusOK
}

// rollupDep is one dependency as the rollup sees it: the child's ID and how the
// edge is classified at this instant.
type rollupDep struct {
	To        string
	Essential bool
}

// depStatus pairs a child's already-computed rollup with its classification.
type depStatus struct {
	Status    Status
	Essential bool
}

// rollupOne is the single-node health rule, and the only place in the entire
// simulation where one node's status is allowed to influence another's.
//
//	local == FAILING                                        -> FAILING
//	any ESSENTIAL child FAILING                              -> FAILING
//	local == DEGRADED
//	  OR any ESSENTIAL child DEGRADED
//	  OR any NON-ESSENTIAL child DEGRADED or FAILING         -> DEGRADED
//	otherwise                                                -> OK
//
// The asymmetry is the thesis of the whole demo: a non-essential dependency can
// colour its parent yellow but can never take it red, no matter how hard it
// fails.
func rollupOne(local Status, deps []depStatus) Status {
	if local == StatusFailing {
		return StatusFailing
	}
	for _, d := range deps {
		if d.Essential && d.Status == StatusFailing {
			return StatusFailing
		}
	}

	if local == StatusDegraded {
		return StatusDegraded
	}
	for _, d := range deps {
		if d.Essential {
			if d.Status == StatusDegraded {
				return StatusDegraded
			}
			continue
		}
		// Non-essential: both DEGRADED and FAILING cap out at DEGRADED here.
		if d.Status == StatusDegraded || d.Status == StatusFailing {
			return StatusDegraded
		}
	}
	return StatusOK
}

// computeRollups evaluates rollupOne across the graph in evalOrder, which must
// be leaves-first (reverse topological). Evaluating in that order means each
// node's children are already final when it is visited, so a shared leaf such as
// artifact-store is computed exactly once and both of its parents observe the
// same value. Unknown children are treated as OK.
func computeRollups(evalOrder []string, local map[string]Status, deps map[string][]rollupDep) map[string]Status {
	out := make(map[string]Status, len(evalOrder))
	for _, id := range evalOrder {
		childStatuses := make([]depStatus, 0, len(deps[id]))
		for _, d := range deps[id] {
			st, ok := out[d.To]
			if !ok {
				// Not yet evaluated (only possible if evalOrder is not
				// leaves-first) or unknown: treat as healthy rather than
				// inventing a failure.
				st = StatusOK
			}
			childStatuses = append(childStatuses, depStatus{Status: st, Essential: d.Essential})
		}
		l, ok := local[id]
		if !ok {
			l = StatusOK
		}
		out[id] = rollupOne(l, childStatuses)
	}
	return out
}

// levelFor maps a status to the UI severity used for transition events.
func levelFor(s Status) EventLevel {
	switch s {
	case StatusFailing:
		return LevelCrit
	case StatusDegraded:
		return LevelWarn
	default:
		return LevelInfo
	}
}
