package sim

import (
	"context"
	"math/rand/v2"
)

// Priority classifies one pipeline run for admission at a gated dependency.
// ReleaseCandidate runs are the ones actually being shipped; everything else
// is Normal. This is the only place a run's priority changes behaviour — see
// service.go's gatedCall.
type Priority string

const (
	PriorityNormal           Priority = "normal"
	PriorityReleaseCandidate Priority = "release_candidate"
)

// rcRatio is the fraction of load-generator traffic tagged ReleaseCandidate.
const rcRatio = 0.10

type priorityCtxKey struct{}

func withPriority(ctx context.Context, p Priority) context.Context {
	return context.WithValue(ctx, priorityCtxKey{}, p)
}

// priorityFromContext defaults to Normal so a call made without an explicit
// priority (a test, for instance) is never accidentally exempt from shedding.
func priorityFromContext(ctx context.Context) Priority {
	if p, ok := ctx.Value(priorityCtxKey{}).(Priority); ok {
		return p
	}
	return PriorityNormal
}

// pickPriority samples one run's priority at the configured rcRatio.
func pickPriority() Priority {
	if rand.Float64() < rcRatio {
		return PriorityReleaseCandidate
	}
	return PriorityNormal
}
