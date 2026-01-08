package auth

import (
	"context"
	"errors"
	"sync"
	"time"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestFillFirstSelectorPick_Deterministic(t *testing.T) {
	t.Parallel()

	selector := &FillFirstSelector{}
	auths := []*Auth{
		{ID: "b"},
		{ID: "a"},
		{ID: "c"},
	}

	got, err := selector.Pick(context.Background(), "gemini", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil {
		t.Fatalf("Pick() auth = nil")
	}
	if got.ID != "a" {
		t.Fatalf("Pick() auth.ID = %q, want %q", got.ID, "a")
	}
}

func TestRoundRobinSelectorPick_CyclesDeterministic(t *testing.T) {
	t.Parallel()

	selector := &RoundRobinSelector{}
	auths := []*Auth{
		{ID: "b"},
		{ID: "a"},
		{ID: "c"},
	}

	want := []string{"a", "b", "c", "a", "b"}
	for i, id := range want {
		got, err := selector.Pick(context.Background(), "gemini", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got == nil {
			t.Fatalf("Pick() #%d auth = nil", i)
		}
		if got.ID != id {
			t.Fatalf("Pick() #%d auth.ID = %q, want %q", i, got.ID, id)
		}
	}
}

func TestRoundRobinSelectorPick_Concurrent(t *testing.T) {
	selector := &RoundRobinSelector{}
	auths := []*Auth{
		{ID: "b"},
		{ID: "a"},
		{ID: "c"},
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, 1)

	goroutines := 32
	iterations := 100
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				got, err := selector.Pick(context.Background(), "gemini", "", cliproxyexecutor.Options{}, auths)
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				if got == nil {
					select {
					case errCh <- errors.New("Pick() returned nil auth"):
					default:
					}
					return
				}
				if got.ID == "" {
					select {
					case errCh <- errors.New("Pick() returned auth with empty ID"):
					default:
					}
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	select {
	case err := <-errCh:
		t.Fatalf("concurrent Pick() error = %v", err)
	default:
	}
}

func TestNewSelector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		strategy string
		wantType string
	}{
		{
			name:     "default round-robin",
			strategy: "",
			wantType: "*auth.RoundRobinSelector",
		},
		{
			name:     "explicit round-robin",
			strategy: "round-robin",
			wantType: "*auth.RoundRobinSelector",
		},
		{
			name:     "fill-first",
			strategy: "fill-first",
			wantType: "*auth.FillFirstSelector",
		},
		{
			name:     "weighted-round-robin",
			strategy: "weighted-round-robin",
			wantType: "*auth.WeightedRoundRobinSelector",
		},
		{
			name:     "unknown defaults to round-robin",
			strategy: "unknown-strategy",
			wantType: "*auth.RoundRobinSelector",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			selector := NewSelector(tc.strategy)
			got := selectorTypeString(selector)
			if got != tc.wantType {
				t.Errorf("NewSelector(%q) = %s, want %s", tc.strategy, got, tc.wantType)
			}
		})
	}
}

func selectorTypeString(s Selector) string {
	switch s.(type) {
	case *RoundRobinSelector:
		return "*auth.RoundRobinSelector"
	case *FillFirstSelector:
		return "*auth.FillFirstSelector"
	case *WeightedRoundRobinSelector:
		return "*auth.WeightedRoundRobinSelector"
	default:
		return "unknown"
	}
}

func TestWeightedRoundRobinSelector_Distribution(t *testing.T) {
	t.Parallel()

	// Create auths with weights: A=3, B=2, C=1
	auths := []*Auth{
		{ID: "a", Provider: "test", Weight: 3, Status: StatusActive},
		{ID: "b", Provider: "test", Weight: 2, Status: StatusActive},
		{ID: "c", Provider: "test", Weight: 1, Status: StatusActive},
	}

	selector := &WeightedRoundRobinSelector{}
	ctx := context.Background()

	// Run many iterations and count selections
	counts := make(map[string]int)
	iterations := 600 // Multiple of total weight (3+2+1=6) for even distribution

	for i := 0; i < iterations; i++ {
		selected, err := selector.Pick(ctx, "test", "model1", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		counts[selected.ID]++
	}

	// With weights 3:2:1, expected distribution is 50%:33%:17%
	// A should get ~3x more than C, B should get ~2x more than C
	if counts["a"] < counts["b"] {
		t.Errorf("a (weight=3) should be selected more than b (weight=2): a=%d, b=%d", counts["a"], counts["b"])
	}
	if counts["b"] < counts["c"] {
		t.Errorf("b (weight=2) should be selected more than c (weight=1): b=%d, c=%d", counts["b"], counts["c"])
	}

	// Check approximate ratios (allow 30% tolerance for algorithm variance)
	expectedRatioAC := 3.0
	actualRatioAC := float64(counts["a"]) / float64(counts["c"])
	if actualRatioAC < expectedRatioAC*0.6 || actualRatioAC > expectedRatioAC*1.4 {
		t.Errorf("a:c ratio = %.2f, expected ~%.2f (counts: a=%d, c=%d)", actualRatioAC, expectedRatioAC, counts["a"], counts["c"])
	}
}

func TestWeightedRoundRobinSelector_DefaultWeight(t *testing.T) {
	t.Parallel()

	// Auths with zero/negative weights should be treated as weight=1
	auths := []*Auth{
		{ID: "a", Provider: "test", Weight: 0, Status: StatusActive},  // should be 1
		{ID: "b", Provider: "test", Weight: -1, Status: StatusActive}, // should be 1
		{ID: "c", Provider: "test", Weight: 2, Status: StatusActive},
	}

	selector := &WeightedRoundRobinSelector{}
	ctx := context.Background()

	counts := make(map[string]int)
	iterations := 400

	for i := 0; i < iterations; i++ {
		selected, err := selector.Pick(ctx, "test", "model1", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		counts[selected.ID]++
	}

	// A and B should have similar counts (both weight=1)
	// C should have ~2x the count of A or B
	diff := absInt(counts["a"] - counts["b"])
	if diff > iterations/5 { // Allow 20% difference
		t.Errorf("a and b should have similar counts: a=%d, b=%d", counts["a"], counts["b"])
	}

	if counts["c"] < counts["a"] {
		t.Errorf("c (weight=2) should be selected more than a (weight=1): c=%d, a=%d", counts["c"], counts["a"])
	}
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func TestWeightedRoundRobinSelector_SingleAuth(t *testing.T) {
	t.Parallel()

	auths := []*Auth{
		{ID: "a", Provider: "test", Weight: 5, Status: StatusActive},
	}

	selector := &WeightedRoundRobinSelector{}
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		selected, err := selector.Pick(ctx, "test", "model1", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		if selected.ID != "a" {
			t.Errorf("Expected a, got %s", selected.ID)
		}
	}
}

func TestWeightedRoundRobinSelector_AuthListChange(t *testing.T) {
	t.Parallel()

	selector := &WeightedRoundRobinSelector{}
	ctx := context.Background()

	// Initial list
	auths1 := []*Auth{
		{ID: "a", Provider: "test", Weight: 2, Status: StatusActive},
		{ID: "b", Provider: "test", Weight: 1, Status: StatusActive},
	}

	// Pick a few times
	for i := 0; i < 3; i++ {
		_, err := selector.Pick(ctx, "test", "model1", cliproxyexecutor.Options{}, auths1)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
	}

	// Change auth list (add c, remove b)
	auths2 := []*Auth{
		{ID: "a", Provider: "test", Weight: 2, Status: StatusActive},
		{ID: "c", Provider: "test", Weight: 3, Status: StatusActive},
	}

	// Should reinitialize and work correctly
	counts := make(map[string]int)
	for i := 0; i < 50; i++ {
		selected, err := selector.Pick(ctx, "test", "model1", cliproxyexecutor.Options{}, auths2)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		counts[selected.ID]++
	}

	// c (weight=3) should get more than a (weight=2)
	if counts["c"] < counts["a"] {
		t.Errorf("c (weight=3) should be selected more than a (weight=2): c=%d, a=%d", counts["c"], counts["a"])
	}
	if counts["b"] != 0 {
		t.Errorf("b should not be selected: b=%d", counts["b"])
	}
}

func TestWeightedRoundRobinSelector_Concurrent(t *testing.T) {
	selector := &WeightedRoundRobinSelector{}
	auths := []*Auth{
		{ID: "a", Provider: "test", Weight: 3, Status: StatusActive},
		{ID: "b", Provider: "test", Weight: 2, Status: StatusActive},
		{ID: "c", Provider: "test", Weight: 1, Status: StatusActive},
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, 1)

	goroutines := 32
	iterations := 100
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				got, err := selector.Pick(context.Background(), "test", "model1", cliproxyexecutor.Options{}, auths)
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				if got == nil {
					select {
					case errCh <- errors.New("Pick() returned nil auth"):
					default:
					}
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	select {
	case err := <-errCh:
		t.Fatalf("concurrent Pick() error = %v", err)
	default:
	}
}

func TestGCD(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a, b, want int
	}{
		{12, 8, 4},
		{100, 25, 25},
		{7, 3, 1},
		{0, 5, 5},
		{5, 0, 5},
		{-12, 8, 4},
		{12, -8, 4},
	}

	for _, tc := range tests {
		got := gcd(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("gcd(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestExtractWeights(t *testing.T) {
	t.Parallel()

	auths := []*Auth{
		{Weight: 5},
		{Weight: 0},
		{Weight: -1},
		{Weight: 3},
	}

	got := extractWeights(auths)
	want := []int{5, 1, 1, 3} // 0 and -1 become 1

	if len(got) != len(want) {
		t.Fatalf("extractWeights() len = %d, want %d", len(got), len(want))
	}

	for i := range got {
		if got[i] != want[i] {
			t.Errorf("extractWeights()[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestLatencyAwareSelectorWithMetrics(t *testing.T) {
	t.Parallel()

	// Create a selector with low threshold for faster testing
	selector := NewLatencyAwareSelector(3)

	auths := []*Auth{
		{ID: "fast", Provider: "test", Status: StatusActive},
		{ID: "medium", Provider: "test", Status: StatusActive},
		{ID: "slow", Provider: "test", Status: StatusActive},
	}

	// Record latency data for all auths
	// Fast: 50ms avg
	// Medium: 150ms avg
	// Slow: 500ms avg
	for i := 0; i < 5; i++ {
		selector.RecordLatency("fast", 50*time.Millisecond, true)
		selector.RecordLatency("medium", 150*time.Millisecond, true)
		selector.RecordLatency("slow", 500*time.Millisecond, true)
	}

	ctx := context.Background()

	// Run many picks and count selections
	counts := make(map[string]int)
	for i := 0; i < 100; i++ {
		selected, err := selector.Pick(ctx, "test", "model1", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		counts[selected.ID]++
	}

	// Fast auth should be selected most often
	if counts["fast"] < counts["slow"] {
		t.Errorf("expected fast auth to be selected more than slow auth: fast=%d, slow=%d", counts["fast"], counts["slow"])
	}
	if counts["fast"] < counts["medium"] {
		t.Errorf("expected fast auth to be selected more than medium auth: fast=%d, medium=%d", counts["fast"], counts["medium"])
	}
	if counts["medium"] < counts["slow"] {
		t.Errorf("expected medium auth to be selected more than slow auth: medium=%d, slow=%d", counts["medium"], counts["slow"])
	}

	// Verify metrics are correctly stored
	avgLatency, successRate, count, ok := selector.GetMetrics("fast")
	if !ok {
		t.Fatal("expected metrics for fast auth")
	}
	if count != 5 {
		t.Errorf("expected count 5, got %d", count)
	}
	if avgLatency != 50*time.Millisecond {
		t.Errorf("expected avg latency 50ms, got %v", avgLatency)
	}
	if successRate != 1.0 {
		t.Errorf("expected success rate 1.0, got %f", successRate)
	}
}

func TestAdaptiveSelectorSwitching(t *testing.T) {
	t.Parallel()

	// Create an adaptive selector with low threshold
	selector := NewAdaptiveSelector(10)

	auths := []*Auth{
		{ID: "fast", Provider: "test", Status: StatusActive},
		{ID: "slow", Provider: "test", Status: StatusActive},
	}

	ctx := context.Background()

	// Initially should use round-robin (even distribution)
	initialCounts := make(map[string]int)
	for i := 0; i < 20; i++ {
		selected, err := selector.Pick(ctx, "test", "model1", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		initialCounts[selected.ID]++
	}

	// Should be roughly even (10 each with perfect round-robin)
	if initialCounts["fast"] < 5 || initialCounts["slow"] < 5 {
		t.Errorf("expected even distribution initially, got %v", initialCounts)
	}

	// Record enough latency data to trigger adaptation
	for i := 0; i < 15; i++ {
		selector.RecordLatency("fast", 50*time.Millisecond, true)
		selector.RecordLatency("slow", 500*time.Millisecond, true)
	}

	// Now should prefer fast auth
	finalCounts := make(map[string]int)
	for i := 0; i < 20; i++ {
		selected, err := selector.Pick(ctx, "test", "model1", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		finalCounts[selected.ID]++
	}

	// After adaptation, fast should be selected more often
	if finalCounts["fast"] < 12 {
		t.Errorf("expected fast auth to be preferred after adaptation, got %v", finalCounts)
	}
	if finalCounts["slow"] > 8 {
		t.Errorf("expected slow auth to be selected less after adaptation, got %v", finalCounts)
	}
}

func TestAdaptiveSelector_MetricsInterface(t *testing.T) {
	t.Parallel()

	selector := NewAdaptiveSelector(20)

	// Record some data
	selector.RecordLatency("auth1", 100*time.Millisecond, true)
	selector.RecordLatency("auth1", 200*time.Millisecond, false) // failure
	selector.RecordLatency("auth2", 300*time.Millisecond, true)

	// Verify metrics are accessible through adaptive selector
	avgLatency, successRate, count, ok := selector.GetMetrics("auth1")
	if !ok {
		t.Fatal("expected metrics for auth1")
	}
	if count != 2 {
		t.Errorf("expected count 2, got %d", count)
	}
	if avgLatency != 150*time.Millisecond {
		t.Errorf("expected avg latency 150ms, got %v", avgLatency)
	}
	expectedRate := 0.5
	if successRate < expectedRate-0.01 || successRate > expectedRate+0.01 {
		t.Errorf("expected success rate ~%.2f, got %.2f", expectedRate, successRate)
	}
}

func TestLatencyAwareSelector_MetricsInterface(t *testing.T) {
	t.Parallel()

	selector := NewLatencyAwareSelector(5)

	// Test metrics for non-existent auth
	_, _, _, ok := selector.GetMetrics("nonexistent")
	if ok {
		t.Error("expected no metrics for nonexistent auth")
	}

	// Test that metrics are correctly updated with failures
	selector.RecordLatency("test-auth", 100*time.Millisecond, true)
	selector.RecordLatency("test-auth", 200*time.Millisecond, false)
	selector.RecordLatency("test-auth", 300*time.Millisecond, false)

	avgLatency, successRate, count, ok := selector.GetMetrics("test-auth")
	if !ok {
		t.Fatal("expected metrics for test-auth")
	}
	if count != 3 {
		t.Errorf("expected count 3, got %d", count)
	}
	expectedAvg := 200 * time.Millisecond
	if avgLatency != expectedAvg {
		t.Errorf("expected avg latency %v, got %v", expectedAvg, avgLatency)
	}
	expectedRate := 1.0 / 3.0
	if successRate < expectedRate-0.01 || successRate > expectedRate+0.01 {
		t.Errorf("expected success rate ~%.2f, got %.2f", expectedRate, successRate)
	}
}
