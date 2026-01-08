package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestCircuitBreaker_InitialState(t *testing.T) {
	cb := NewCircuitBreaker(DefaultCircuitBreakerConfig())
	if cb.State() != CircuitClosed {
		t.Errorf("expected initial state to be closed, got %v", cb.State())
	}
}

func TestCircuitBreaker_OpensAfterFailures(t *testing.T) {
	config := CircuitBreakerConfig{
		FailureThreshold:    3,
		SuccessThreshold:    2,
		OpenTimeout:         100 * time.Millisecond,
		HalfOpenMaxRequests: 1,
	}
	cb := NewCircuitBreaker(config)

	// Record failures up to threshold
	for i := 0; i < 3; i++ {
		if !cb.Allow() {
			t.Fatalf("expected request %d to be allowed", i)
		}
		cb.RecordFailure()
	}

	// Circuit should now be open
	if cb.State() != CircuitOpen {
		t.Errorf("expected state to be open after %d failures, got %v", 3, cb.State())
	}

	// Requests should be blocked
	if cb.Allow() {
		t.Error("expected request to be blocked when circuit is open")
	}
}

func TestCircuitBreaker_TransitionsToHalfOpen(t *testing.T) {
	config := CircuitBreakerConfig{
		FailureThreshold:    2,
		SuccessThreshold:    2,
		OpenTimeout:         20 * time.Millisecond,
		HalfOpenMaxRequests: 1,
	}
	cb := NewCircuitBreaker(config)

	// Open the circuit by recording failures with Allow() calls
	cb.Allow()
	cb.RecordFailure()
	cb.Allow()
	cb.RecordFailure()

	if cb.State() != CircuitOpen {
		t.Fatalf("expected circuit to be open, got %v", cb.State())
	}

	// Verify circuit is blocking requests
	if cb.Allow() {
		t.Error("expected request to be blocked when circuit is open")
	}

	// Wait for timeout (with extra margin)
	time.Sleep(100 * time.Millisecond)

	// Should transition to half-open on next Allow
	allowed := cb.Allow()
	state := cb.State()

	if !allowed {
		t.Errorf("expected request to be allowed after timeout, state=%v", state)
	}

	if state != CircuitHalfOpen {
		t.Errorf("expected state to be half-open, got %v", state)
	}
}

func TestCircuitBreaker_ClosesAfterSuccesses(t *testing.T) {
	config := CircuitBreakerConfig{
		FailureThreshold:    2,
		SuccessThreshold:    2,
		OpenTimeout:         20 * time.Millisecond,
		HalfOpenMaxRequests: 3,
	}
	cb := NewCircuitBreaker(config)

	// Open the circuit
	cb.Allow()
	cb.RecordFailure()
	cb.Allow()
	cb.RecordFailure()

	if cb.State() != CircuitOpen {
		t.Fatalf("expected circuit to be open, got %v", cb.State())
	}

	// Wait for timeout (with extra margin)
	time.Sleep(100 * time.Millisecond)

	// Transition to half-open
	if !cb.Allow() {
		t.Fatal("expected first request to be allowed in half-open")
	}

	if cb.State() != CircuitHalfOpen {
		t.Fatalf("expected state to be half-open, got %v", cb.State())
	}

	// Record successes
	cb.RecordSuccess()

	if !cb.Allow() {
		t.Fatal("expected second request to be allowed in half-open")
	}
	cb.RecordSuccess()

	// Should be closed now
	if cb.State() != CircuitClosed {
		t.Errorf("expected state to be closed after successes, got %v", cb.State())
	}
}

func TestCircuitBreaker_HalfOpenFailureReopens(t *testing.T) {
	config := CircuitBreakerConfig{
		FailureThreshold:    2,
		SuccessThreshold:    2,
		OpenTimeout:         10 * time.Millisecond,
		HalfOpenMaxRequests: 1,
	}
	cb := NewCircuitBreaker(config)

	// Open the circuit
	cb.RecordFailure()
	cb.RecordFailure()

	// Wait for timeout
	time.Sleep(20 * time.Millisecond)

	// Transition to half-open
	cb.Allow()

	// Record failure in half-open
	cb.RecordFailure()

	// Should be open again
	if cb.State() != CircuitOpen {
		t.Errorf("expected state to be open after half-open failure, got %v", cb.State())
	}
}

func TestCircuitBreaker_Concurrent(t *testing.T) {
	config := CircuitBreakerConfig{
		FailureThreshold:    100,
		SuccessThreshold:    10,
		OpenTimeout:         time.Second,
		HalfOpenMaxRequests: 5,
	}
	cb := NewCircuitBreaker(config)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if cb.Allow() {
				if i%2 == 0 {
					cb.RecordSuccess()
				} else {
					cb.RecordFailure()
				}
			}
		}()
	}
	wg.Wait()

	// Just verify no panics occurred
	_ = cb.Stats()
}

func TestCircuitBreakerManager(t *testing.T) {
	manager := NewCircuitBreakerManager(DefaultCircuitBreakerConfig())

	// Test getting breakers for different auth IDs
	b1 := manager.GetBreaker("auth1")
	b2 := manager.GetBreaker("auth2")
	b1Again := manager.GetBreaker("auth1")

	if b1 != b1Again {
		t.Error("expected same breaker for same auth ID")
	}
	if b1 == b2 {
		t.Error("expected different breakers for different auth IDs")
	}

	// Test recording
	manager.RecordSuccess("auth1")
	manager.RecordFailure("auth2")

	stats := manager.Stats()
	if len(stats) != 2 {
		t.Errorf("expected 2 stats entries, got %d", len(stats))
	}
}

func TestLatencyAwareSelector_FallbackToRoundRobin(t *testing.T) {
	selector := NewLatencyAwareSelector(10)

	auths := []*Auth{
		{ID: "auth1", Provider: "test"},
		{ID: "auth2", Provider: "test"},
		{ID: "auth3", Provider: "test"},
	}

	// Without enough samples, should use round-robin
	ctx := context.Background()
	selected1, err := selector.Pick(ctx, "test", "model", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected1 == nil {
		t.Fatal("expected non-nil selection")
	}

	selected2, _ := selector.Pick(ctx, "test", "model", cliproxyexecutor.Options{}, auths)
	selected3, _ := selector.Pick(ctx, "test", "model", cliproxyexecutor.Options{}, auths)

	// Should cycle through auths (round-robin behavior)
	if selected1.ID == selected2.ID && selected2.ID == selected3.ID {
		t.Error("expected round-robin to cycle through different auths")
	}
}

func TestLatencyAwareSelector_UsesLatencyData(t *testing.T) {
	selector := NewLatencyAwareSelector(2) // Low threshold for testing

	auths := []*Auth{
		{ID: "fast", Provider: "test"},
		{ID: "slow", Provider: "test"},
	}

	// Record latency data
	for i := 0; i < 5; i++ {
		selector.RecordLatency("fast", 50*time.Millisecond, true)
		selector.RecordLatency("slow", 500*time.Millisecond, true)
	}

	// Now selector should prefer the faster auth
	ctx := context.Background()
	fastCount := 0
	for i := 0; i < 10; i++ {
		selected, _ := selector.Pick(ctx, "test", "model", cliproxyexecutor.Options{}, auths)
		if selected.ID == "fast" {
			fastCount++
		}
	}

	// Fast auth should be selected more often
	if fastCount < 7 {
		t.Errorf("expected fast auth to be selected more often, got %d/10", fastCount)
	}
}

func TestLatencyAwareSelector_GetMetrics(t *testing.T) {
	selector := NewLatencyAwareSelector(5)

	// Record some data
	selector.RecordLatency("auth1", 100*time.Millisecond, true)
	selector.RecordLatency("auth1", 200*time.Millisecond, true)
	selector.RecordLatency("auth1", 300*time.Millisecond, false)

	avgLatency, successRate, count, ok := selector.GetMetrics("auth1")
	if !ok {
		t.Fatal("expected metrics to exist")
	}

	if count != 3 {
		t.Errorf("expected count 3, got %d", count)
	}

	expectedAvg := 200 * time.Millisecond
	if avgLatency != expectedAvg {
		t.Errorf("expected avg latency %v, got %v", expectedAvg, avgLatency)
	}

	expectedRate := 2.0 / 3.0
	if successRate < expectedRate-0.01 || successRate > expectedRate+0.01 {
		t.Errorf("expected success rate ~%.2f, got %.2f", expectedRate, successRate)
	}

	// Non-existent auth
	_, _, _, ok = selector.GetMetrics("nonexistent")
	if ok {
		t.Error("expected no metrics for nonexistent auth")
	}
}

func TestAdaptiveSelector_StartsWithRoundRobin(t *testing.T) {
	selector := NewAdaptiveSelector(100) // High threshold

	auths := []*Auth{
		{ID: "auth1", Provider: "test"},
		{ID: "auth2", Provider: "test"},
	}

	ctx := context.Background()

	// Should use round-robin initially
	selections := make(map[string]int)
	for i := 0; i < 10; i++ {
		selected, _ := selector.Pick(ctx, "test", "model", cliproxyexecutor.Options{}, auths)
		selections[selected.ID]++
	}

	// Should be roughly even distribution
	if selections["auth1"] < 3 || selections["auth2"] < 3 {
		t.Errorf("expected even distribution, got %v", selections)
	}
}

func TestAdaptiveSelector_SwitchesToLatencyAware(t *testing.T) {
	selector := NewAdaptiveSelector(10) // Low threshold

	auths := []*Auth{
		{ID: "fast", Provider: "test"},
		{ID: "slow", Provider: "test"},
	}

	// Record enough data to trigger adaptation
	for i := 0; i < 15; i++ {
		selector.RecordLatency("fast", 50*time.Millisecond, true)
		selector.RecordLatency("slow", 500*time.Millisecond, true)
	}

	ctx := context.Background()
	fastCount := 0
	for i := 0; i < 10; i++ {
		selected, _ := selector.Pick(ctx, "test", "model", cliproxyexecutor.Options{}, auths)
		if selected.ID == "fast" {
			fastCount++
		}
	}

	// After adaptation, should prefer faster auth
	if fastCount < 7 {
		t.Errorf("expected fast auth to be preferred after adaptation, got %d/10", fastCount)
	}
}

func TestNewSelector_NewStrategies(t *testing.T) {
	tests := []struct {
		strategy string
		expected string
	}{
		{"latency-aware", "*auth.LatencyAwareSelector"},
		{"adaptive", "*auth.AdaptiveSelector"},
	}

	for _, tt := range tests {
		t.Run(tt.strategy, func(t *testing.T) {
			selector := NewSelector(tt.strategy)
			if selector == nil {
				t.Fatalf("expected non-nil selector for strategy %s", tt.strategy)
			}
		})
	}
}
