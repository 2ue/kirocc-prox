package pool

import (
	"errors"
	"sync"
)

// ErrNoReady is returned by Selector.Pick when no credentials are selectable.
var ErrNoReady = errors.New("pool: no ready credentials")

// Selector chooses one credential from the ready set. The model parameter
// allows per-model cooldowns to be considered: a credential whose
// per-model QuotaState is still in cooldown is skipped even if its
// account-level state is clean.
//
// Implementations must be safe for concurrent use.
type Selector interface {
	Pick(ready []*Credential, model string) (*Credential, error)
}

// NewSelector returns the configured scheduling strategy. Unknown values fall
// back to round-robin; Config.Validate is responsible for rejecting invalid
// operator input before production startup.
func NewSelector(strategy string) Selector {
	switch strategy {
	case "fill-first":
		return &FillFirstSelector{}
	case "least-used":
		return &LeastUsedSelector{}
	case "least-inflight":
		return &LeastInFlightSelector{}
	case "weighted-least-inflight":
		return &WeightedLeastInFlightSelector{}
	default:
		return &RoundRobinSelector{}
	}
}

// RoundRobinSelector cycles through ready credentials in priority order.
// The cursor advances on each Pick; it is reset when the ready set
// composition changes (additions / removals).
type RoundRobinSelector struct {
	mu     sync.Mutex
	cursor int
}

// FillFirstSelector picks the highest-priority ready credential and stays
// on it until that credential goes into cooldown or is disabled. Good for
// tier-1 plans that should drain before tier-2 starts.
type FillFirstSelector struct{}

// LeastUsedSelector picks the ready credential with the smallest Success
// counter (after grouping by Priority). Spreads load across credentials
// over time.
type LeastUsedSelector struct{}

// LeastInFlightSelector picks the ready credential with the fewest currently
// running requests inside the top-priority group.
type LeastInFlightSelector struct{}

// WeightedLeastInFlightSelector picks the ready credential with the lowest
// in-flight / max-in-flight load ratio inside the top-priority group. Accounts
// without a max cap are treated as having a very large capacity.
type WeightedLeastInFlightSelector struct{}

// Pick methods on these selectors live in selector_strategies.go.
