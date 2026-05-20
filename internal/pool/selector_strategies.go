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

// snapshotSuccess returns the Success counter under a brief read lock.
func snapshotSuccess(c *Credential) int64 {
	c.Mu.RLock()
	defer c.Mu.RUnlock()
	return c.Success
}
