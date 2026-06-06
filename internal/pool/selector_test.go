package pool

import (
	"errors"
	"testing"
)

func mkCred(id string, priority int, success int64) *Credential {
	return &Credential{
		ID:       id,
		Priority: priority,
		Success:  success,
	}
}

func TestRoundRobinSelector_Cycles(t *testing.T) {
	ready := []*Credential{mkCred("a", 100, 0), mkCred("b", 100, 0), mkCred("c", 100, 0)}
	s := &RoundRobinSelector{}
	want := []string{"a", "b", "c", "a", "b", "c", "a"}
	for i, w := range want {
		got, err := s.Pick(ready, "")
		if err != nil {
			t.Fatalf("iter %d: unexpected err: %v", i, err)
		}
		if got.ID != w {
			t.Errorf("iter %d: got %q want %q", i, got.ID, w)
		}
	}
}

func TestRoundRobinSelector_Empty(t *testing.T) {
	s := &RoundRobinSelector{}
	if _, err := s.Pick(nil, ""); !errors.Is(err, ErrNoReady) {
		t.Errorf("expected ErrNoReady, got %v", err)
	}
	if _, err := s.Pick([]*Credential{}, ""); !errors.Is(err, ErrNoReady) {
		t.Errorf("expected ErrNoReady, got %v", err)
	}
}

func TestRoundRobinSelector_CursorReset(t *testing.T) {
	s := &RoundRobinSelector{}
	full := []*Credential{mkCred("a", 100, 0), mkCred("b", 100, 0), mkCred("c", 100, 0)}
	// Advance cursor.
	_, _ = s.Pick(full, "")
	_, _ = s.Pick(full, "")
	_, _ = s.Pick(full, "")
	// Now shrink the ready set; cursor (=0 from wrap) should still work.
	small := []*Credential{mkCred("x", 100, 0)}
	got, err := s.Pick(small, "")
	if err != nil {
		t.Fatalf("after shrink: %v", err)
	}
	if got.ID != "x" {
		t.Errorf("after shrink got %q want x", got.ID)
	}
}

func TestFillFirstSelector_StaysOnFirst(t *testing.T) {
	ready := []*Credential{mkCred("a", 100, 0), mkCred("b", 90, 0), mkCred("c", 80, 0)}
	s := &FillFirstSelector{}
	for i := 0; i < 5; i++ {
		got, err := s.Pick(ready, "")
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if got.ID != "a" {
			t.Errorf("iter %d: got %q want a", i, got.ID)
		}
	}
}

func TestFillFirstSelector_Empty(t *testing.T) {
	s := &FillFirstSelector{}
	if _, err := s.Pick(nil, ""); !errors.Is(err, ErrNoReady) {
		t.Errorf("expected ErrNoReady, got %v", err)
	}
}

func TestLeastUsedSelector_PicksMinSuccessInTopGroup(t *testing.T) {
	// Top priority group (100): b has fewer successes than a.
	// Lower priority (50): c is ignored even though it's the global min.
	ready := []*Credential{
		mkCred("a", 100, 5),
		mkCred("b", 100, 2),
		mkCred("c", 50, 0),
	}
	s := &LeastUsedSelector{}
	got, err := s.Pick(ready, "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.ID != "b" {
		t.Errorf("got %q want b", got.ID)
	}
}

func TestLeastUsedSelector_Empty(t *testing.T) {
	s := &LeastUsedSelector{}
	if _, err := s.Pick(nil, ""); !errors.Is(err, ErrNoReady) {
		t.Errorf("expected ErrNoReady, got %v", err)
	}
}

func TestLeastUsedSelector_Single(t *testing.T) {
	ready := []*Credential{mkCred("only", 100, 42)}
	s := &LeastUsedSelector{}
	got, err := s.Pick(ready, "")
	if err != nil {
		t.Fatalf("%v", err)
	}
	if got.ID != "only" {
		t.Errorf("got %q want only", got.ID)
	}
}

func TestLeastInFlightSelector_PicksMinInFlightInTopGroup(t *testing.T) {
	ready := []*Credential{
		mkCred("a", 100, 0),
		mkCred("b", 100, 5),
		mkCred("c", 50, 0),
	}
	ready[0].InFlight = 2
	ready[1].InFlight = 1
	ready[2].InFlight = 0

	s := &LeastInFlightSelector{}
	got, err := s.Pick(ready, "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.ID != "b" {
		t.Errorf("got %q want b", got.ID)
	}
}

func TestLeastInFlightSelector_TieBreaksBySuccess(t *testing.T) {
	ready := []*Credential{
		mkCred("a", 100, 5),
		mkCred("b", 100, 2),
	}
	ready[0].InFlight = 1
	ready[1].InFlight = 1

	s := &LeastInFlightSelector{}
	got, err := s.Pick(ready, "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.ID != "b" {
		t.Errorf("got %q want b", got.ID)
	}
}

func TestLeastInFlightSelector_Empty(t *testing.T) {
	s := &LeastInFlightSelector{}
	if _, err := s.Pick(nil, ""); !errors.Is(err, ErrNoReady) {
		t.Errorf("expected ErrNoReady, got %v", err)
	}
}

func TestWeightedLeastInFlightSelector_PicksLowestLoadRatio(t *testing.T) {
	ready := []*Credential{
		mkCred("a", 100, 0),
		mkCred("b", 100, 0),
		mkCred("c", 50, 0),
	}
	ready[0].MaxInFlight = 2
	ready[0].InFlight = 1
	ready[1].MaxInFlight = 8
	ready[1].InFlight = 2
	ready[2].MaxInFlight = 10
	ready[2].InFlight = 0

	s := &WeightedLeastInFlightSelector{}
	got, err := s.Pick(ready, "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.ID != "b" {
		t.Errorf("got %q want b", got.ID)
	}
}

func TestWeightedLeastInFlightSelector_Empty(t *testing.T) {
	s := &WeightedLeastInFlightSelector{}
	if _, err := s.Pick(nil, ""); !errors.Is(err, ErrNoReady) {
		t.Errorf("expected ErrNoReady, got %v", err)
	}
}

func TestNewSelector_InFlightStrategies(t *testing.T) {
	if _, ok := NewSelector("least-inflight").(*LeastInFlightSelector); !ok {
		t.Fatalf("least-inflight did not create LeastInFlightSelector")
	}
	if _, ok := NewSelector("weighted-least-inflight").(*WeightedLeastInFlightSelector); !ok {
		t.Fatalf("weighted-least-inflight did not create WeightedLeastInFlightSelector")
	}
}
