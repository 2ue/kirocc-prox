package pool

// Pick cycles through the ready set with a mutex-guarded cursor. The cursor
// is reset whenever it falls outside the bounds of the current ready slice
// (handles set composition changes between calls).
func (s *RoundRobinSelector) Pick(ready []*Credential, model string) (*Credential, error) {
	if len(ready) == 0 {
		return nil, ErrNoReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursor < 0 || s.cursor >= len(ready) {
		s.cursor = 0
	}
	c := ready[s.cursor]
	s.cursor = (s.cursor + 1) % len(ready)
	return c, nil
}

// Pick returns the first ready credential. Since Scheduler.Ready() sorts by
// Priority descending, this is the highest-priority ready credential.
func (s *FillFirstSelector) Pick(ready []*Credential, model string) (*Credential, error) {
	if len(ready) == 0 {
		return nil, ErrNoReady
	}
	return ready[0], nil
}

// Pick returns, within the highest-priority group, the credential with the
// smallest Success counter. The ready slice is assumed to be sorted by
// Priority descending (the contract from Scheduler.Ready()).
func (s *LeastUsedSelector) Pick(ready []*Credential, model string) (*Credential, error) {
	if len(ready) == 0 {
		return nil, ErrNoReady
	}
	topPrio := ready[0].Priority
	best := ready[0]
	bestSuccess := snapshotSuccess(best)
	for i := 1; i < len(ready); i++ {
		c := ready[i]
		if c.Priority != topPrio {
			break
		}
		s := snapshotSuccess(c)
		if s < bestSuccess {
			best = c
			bestSuccess = s
		}
	}
	return best, nil
}

// Pick returns, within the highest-priority group, the credential with the
// fewest currently in-flight requests.
func (s *LeastInFlightSelector) Pick(ready []*Credential, model string) (*Credential, error) {
	if len(ready) == 0 {
		return nil, ErrNoReady
	}
	topPrio := ready[0].Priority
	best := ready[0]
	bestInFlight, bestSuccess := snapshotLoad(best)
	for i := 1; i < len(ready); i++ {
		c := ready[i]
		if c.Priority != topPrio {
			break
		}
		inFlight, success := snapshotLoad(c)
		if inFlight < bestInFlight || (inFlight == bestInFlight && success < bestSuccess) {
			best = c
			bestInFlight = inFlight
			bestSuccess = success
		}
	}
	return best, nil
}

// Pick returns, within the highest-priority group, the credential with the
// lowest in-flight/capacity ratio. Ties fall back to raw in-flight count and
// then Success to keep long-running load even.
func (s *WeightedLeastInFlightSelector) Pick(ready []*Credential, model string) (*Credential, error) {
	if len(ready) == 0 {
		return nil, ErrNoReady
	}
	topPrio := ready[0].Priority
	best := ready[0]
	bestScore := snapshotLoadScore(best)
	for i := 1; i < len(ready); i++ {
		c := ready[i]
		if c.Priority != topPrio {
			break
		}
		score := snapshotLoadScore(c)
		if score.less(bestScore) {
			best = c
			bestScore = score
		}
	}
	return best, nil
}

// snapshotSuccess returns the Success counter under a brief read lock.
func snapshotSuccess(c *Credential) int64 {
	c.Mu.RLock()
	defer c.Mu.RUnlock()
	return c.Success
}

func snapshotLoad(c *Credential) (inFlight, success int64) {
	c.Mu.RLock()
	defer c.Mu.RUnlock()
	return c.InFlight, c.Success
}

type loadScore struct {
	inFlight int64
	capacity int64
	success  int64
}

func snapshotLoadScore(c *Credential) loadScore {
	c.Mu.RLock()
	defer c.Mu.RUnlock()
	capacity := int64(c.MaxInFlight)
	if capacity <= 0 {
		capacity = 1 << 30
	}
	return loadScore{inFlight: c.InFlight, capacity: capacity, success: c.Success}
}

func (s loadScore) less(other loadScore) bool {
	left := s.inFlight * other.capacity
	right := other.inFlight * s.capacity
	if left != right {
		return left < right
	}
	if s.inFlight != other.inFlight {
		return s.inFlight < other.inFlight
	}
	return s.success < other.success
}
