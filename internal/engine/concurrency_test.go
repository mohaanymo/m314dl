package engine

import (
	"context"
	"sync"
	"testing"
)

func TestDecideSlowStart(t *testing.T) {
	// Rising throughput doubles the limit up to the ceiling.
	s := ctlState{slowStart: true}
	limit := adaptiveStart
	tp := 100.0
	for i := 0; i < 10; i++ {
		limit, s = decide(limit, tp, 0, adaptiveMin, adaptiveMax, s)
		tp *= 2 // keep improving
	}
	if limit != adaptiveMax {
		t.Fatalf("slow-start should reach max %d, got %d", adaptiveMax, limit)
	}
}

func TestDecidePlateauHolds(t *testing.T) {
	// Once throughput stops improving, slow-start ends and the limit holds.
	s := ctlState{slowStart: true, prevTP: 1000}
	limit := 32
	limit, s = decide(limit, 1000, 0, adaptiveMin, adaptiveMax, s) // flat → exit slow start
	if s.slowStart {
		t.Fatal("slow-start should have ended on plateau")
	}
	got, _ := decide(limit, 1000, 0, adaptiveMin, adaptiveMax, s) // still flat → hold
	if got != limit {
		t.Fatalf("plateau should hold at %d, moved to %d", limit, got)
	}
}

func TestDecideErrorHalves(t *testing.T) {
	s := ctlState{slowStart: false, prevTP: 1000}
	got, ns := decide(40, 1000, 3, adaptiveMin, adaptiveMax, s)
	if got != 20 {
		t.Fatalf("error should halve 40→20, got %d", got)
	}
	if ns.cooldown == 0 {
		t.Fatal("error should trigger cooldown")
	}
}

func TestDecideRespectsMin(t *testing.T) {
	s := ctlState{slowStart: false}
	got, _ := decide(adaptiveMin, 100, 5, adaptiveMin, adaptiveMax, s)
	if got < adaptiveMin {
		t.Fatalf("must not drop below min %d, got %d", adaptiveMin, got)
	}
}

func TestDecideRespectsCeiling(t *testing.T) {
	// -t ceiling of 8: slow-start with rising throughput never exceeds 8.
	s := ctlState{slowStart: true}
	limit := 8
	tp := 100.0
	for i := 0; i < 6; i++ {
		limit, s = decide(limit, tp, 0, adaptiveMin, 8, s)
		tp *= 2
	}
	if limit > 8 {
		t.Fatalf("must not exceed ceiling 8, got %d", limit)
	}
}

func TestNewControllerCeiling(t *testing.T) {
	c := newController(8)
	if c.maxC != 8 || c.lim.getLimit() > 8 {
		t.Fatalf("ceiling 8: maxC=%d start=%d", c.maxC, c.lim.getLimit())
	}
	// unset (0) falls back to the default ceiling
	if newController(0).maxC != adaptiveMax {
		t.Fatal("ceiling 0 should default to adaptiveMax")
	}
}

func TestLimiterResizeAndCancel(t *testing.T) {
	l := newLimiter(2)
	ctx, cancel := context.WithCancel(context.Background())
	// fill both slots
	if err := l.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	// a third acquire blocks; grow the limit and it proceeds
	done := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); done <- l.acquire(ctx) }()
	l.setLimit(3) // wakes the waiter
	if err := <-done; err != nil {
		t.Fatalf("acquire after grow should succeed: %v", err)
	}
	wg.Wait()

	// a fourth blocks again (limit 3, inflight 3); cancel unblocks it
	go func() { done <- l.acquire(ctx) }()
	cancel()
	l.unblock()
	if err := <-done; err == nil {
		t.Fatal("acquire should fail after cancel")
	}
}
