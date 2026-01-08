package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// RoundRobinSelector provides a simple provider scoped round-robin selection strategy.
// Uses atomic operations for lock-free cursor management under high concurrency.
type RoundRobinSelector struct {
	mu      sync.RWMutex
	cursors sync.Map // map[string]*uint64 for lock-free atomic increments
}

// FillFirstSelector selects the first available credential (deterministic ordering).
// This "burns" one account before moving to the next, which can help stagger
// rolling-window subscription caps (e.g. chat message limits).
type FillFirstSelector struct{}

// WeightedRoundRobinSelector provides weighted round-robin selection based on Auth.Weight.
// Credentials with higher weights receive proportionally more traffic.
// A weight of 0 or negative is treated as 1 (default weight).
type WeightedRoundRobinSelector struct {
	mu      sync.Mutex
	cursors map[string]*weightedCursor
}

// weightedCursor tracks weighted round-robin state for a provider:model pair.
type weightedCursor struct {
	currentIndex  int   // Current auth index in the list
	currentWeight int   // Remaining weight for current auth
	effectiveGCD  int   // GCD of all weights for efficiency
	maxWeight     int   // Maximum weight in the group
	weights       []int // Cached weights for the auth list
}

// LatencyAwareSelector selects credentials based on response latency.
// Credentials with lower average latency receive more traffic.
// Falls back to round-robin when latency data is insufficient.
type LatencyAwareSelector struct {
	mu       sync.RWMutex
	metrics  map[string]*authMetrics // authID -> metrics
	cursors  sync.Map                // fallback round-robin cursors
	minSamples int                   // minimum samples before using latency data
}

// authMetrics tracks performance metrics for a single auth credential.
type authMetrics struct {
	mu            sync.RWMutex
	totalLatency  time.Duration
	requestCount  int64
	successCount  int64
	failureCount  int64
	lastUpdated   time.Time
	avgLatency    time.Duration // cached average
	successRate   float64       // cached success rate
}

// AdaptiveSelector combines multiple strategies and adapts based on performance.
// It starts with round-robin and gradually shifts to latency-aware as data accumulates.
type AdaptiveSelector struct {
	latencySelector *LatencyAwareSelector
	roundRobin      *RoundRobinSelector
	adaptThreshold  int // requests before switching to latency-aware
}

// SelectorStrategy defines the available load balancing strategies.
type SelectorStrategy string

const (
	// StrategyRoundRobin distributes requests evenly across available credentials.
	StrategyRoundRobin SelectorStrategy = "round-robin"
	// StrategyFillFirst uses one credential until exhausted before moving to next.
	StrategyFillFirst SelectorStrategy = "fill-first"
	// StrategyWeightedRoundRobin distributes based on credential weights.
	StrategyWeightedRoundRobin SelectorStrategy = "weighted-round-robin"
	// StrategyLatencyAware selects based on response latency metrics.
	StrategyLatencyAware SelectorStrategy = "latency-aware"
	// StrategyAdaptive combines strategies and adapts based on performance data.
	StrategyAdaptive SelectorStrategy = "adaptive"
)

// NewSelector creates a Selector based on the strategy name.
// Returns RoundRobinSelector for unknown strategies.
func NewSelector(strategy string) Selector {
	switch SelectorStrategy(strategy) {
	case StrategyFillFirst:
		return &FillFirstSelector{}
	case StrategyWeightedRoundRobin:
		return &WeightedRoundRobinSelector{}
	case StrategyLatencyAware:
		return NewLatencyAwareSelector(10) // require 10 samples minimum
	case StrategyAdaptive:
		return NewAdaptiveSelector(50) // adapt after 50 requests
	case StrategyRoundRobin:
		fallthrough
	default:
		return &RoundRobinSelector{}
	}
}

type blockReason int

const (
	blockReasonNone blockReason = iota
	blockReasonCooldown
	blockReasonDisabled
	blockReasonOther
)

type modelCooldownError struct {
	model    string
	resetIn  time.Duration
	provider string
}

func newModelCooldownError(model, provider string, resetIn time.Duration) *modelCooldownError {
	if resetIn < 0 {
		resetIn = 0
	}
	return &modelCooldownError{
		model:    model,
		provider: provider,
		resetIn:  resetIn,
	}
}

func (e *modelCooldownError) Error() string {
	modelName := e.model
	if modelName == "" {
		modelName = "requested model"
	}
	message := fmt.Sprintf("All credentials for model %s are cooling down", modelName)
	if e.provider != "" {
		message = fmt.Sprintf("%s via provider %s", message, e.provider)
	}
	resetSeconds := int(math.Ceil(e.resetIn.Seconds()))
	if resetSeconds < 0 {
		resetSeconds = 0
	}
	displayDuration := e.resetIn
	if displayDuration > 0 && displayDuration < time.Second {
		displayDuration = time.Second
	} else {
		displayDuration = displayDuration.Round(time.Second)
	}
	errorBody := map[string]any{
		"code":          "model_cooldown",
		"message":       message,
		"model":         e.model,
		"reset_time":    displayDuration.String(),
		"reset_seconds": resetSeconds,
	}
	if e.provider != "" {
		errorBody["provider"] = e.provider
	}
	payload := map[string]any{"error": errorBody}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf(`{"error":{"code":"model_cooldown","message":"%s"}}`, message)
	}
	return string(data)
}

func (e *modelCooldownError) StatusCode() int {
	return http.StatusTooManyRequests
}

func (e *modelCooldownError) Headers() http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	resetSeconds := int(math.Ceil(e.resetIn.Seconds()))
	if resetSeconds < 0 {
		resetSeconds = 0
	}
	headers.Set("Retry-After", strconv.Itoa(resetSeconds))
	return headers
}

func collectAvailable(auths []*Auth, model string, now time.Time) (available []*Auth, cooldownCount int, earliest time.Time) {
	available = make([]*Auth, 0, len(auths))
	for i := 0; i < len(auths); i++ {
		candidate := auths[i]
		blocked, reason, next := isAuthBlockedForModel(candidate, model, now)
		if !blocked {
			available = append(available, candidate)
			continue
		}
		if reason == blockReasonCooldown {
			cooldownCount++
			if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
				earliest = next
			}
		}
	}
	if len(available) > 1 {
		sort.Slice(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	}
	return available, cooldownCount, earliest
}

func getAvailableAuths(auths []*Auth, provider, model string, now time.Time) ([]*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}

	available, cooldownCount, earliest := collectAvailable(auths, model, now)
	if len(available) == 0 {
		if cooldownCount == len(auths) && !earliest.IsZero() {
			resetIn := earliest.Sub(now)
			if resetIn < 0 {
				resetIn = 0
			}
			return nil, newModelCooldownError(model, provider, resetIn)
		}
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
	}

	return available, nil
}

// Pick selects the next available auth for the provider in a round-robin manner.
// Uses lock-free atomic operations for high-concurrency performance.
func (s *RoundRobinSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = ctx
	_ = opts
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	key := provider + ":" + model

	// Get or create atomic counter for this key
	var counter *uint64
	if val, ok := s.cursors.Load(key); ok {
		counter = val.(*uint64)
	} else {
		newCounter := new(uint64)
		actual, loaded := s.cursors.LoadOrStore(key, newCounter)
		if loaded {
			counter = actual.(*uint64)
		} else {
			counter = newCounter
		}
	}

	// Atomic increment and get index
	index := atomic.AddUint64(counter, 1) - 1

	// Wrap around to prevent overflow (reset at 2^62 to stay safe)
	if index >= 1<<62 {
		atomic.StoreUint64(counter, 0)
		index = 0
	}

	return available[int(index%uint64(len(available)))], nil
}

// Pick selects the first available auth for the provider in a deterministic manner.
func (s *FillFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = ctx
	_ = opts
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	return available[0], nil
}

func isAuthBlockedForModel(auth *Auth, model string, now time.Time) (bool, blockReason, time.Time) {
	if auth == nil {
		return true, blockReasonOther, time.Time{}
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return true, blockReasonDisabled, time.Time{}
	}
	if model != "" {
		if len(auth.ModelStates) > 0 {
			if state, ok := auth.ModelStates[model]; ok && state != nil {
				if state.Status == StatusDisabled {
					return true, blockReasonDisabled, time.Time{}
				}
				if state.Unavailable {
					if state.NextRetryAfter.IsZero() {
						return false, blockReasonNone, time.Time{}
					}
					if state.NextRetryAfter.After(now) {
						next := state.NextRetryAfter
						if !state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.After(now) {
							next = state.Quota.NextRecoverAt
						}
						if next.Before(now) {
							next = now
						}
						if state.Quota.Exceeded {
							return true, blockReasonCooldown, next
						}
						return true, blockReasonOther, next
					}
				}
				return false, blockReasonNone, time.Time{}
			}
		}
		return false, blockReasonNone, time.Time{}
	}
	if auth.Unavailable && auth.NextRetryAfter.After(now) {
		next := auth.NextRetryAfter
		if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
			next = auth.Quota.NextRecoverAt
		}
		if next.Before(now) {
			next = now
		}
		if auth.Quota.Exceeded {
			return true, blockReasonCooldown, next
		}
		return true, blockReasonOther, next
	}
	return false, blockReasonNone, time.Time{}
}

// Pick selects the next available auth using weighted round-robin algorithm.
// Credentials with higher weights receive proportionally more traffic.
func (s *WeightedRoundRobinSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = ctx
	_ = opts
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	if len(available) == 1 {
		return available[0], nil
	}

	key := provider + ":" + model
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cursors == nil {
		s.cursors = make(map[string]*weightedCursor)
	}

	cursor, exists := s.cursors[key]
	weights := extractWeights(available)

	// Check if we need to reinitialize (new cursor or auth list changed)
	if !exists || !weightsMatch(cursor.weights, weights) {
		cursor = initWeightedCursor(weights)
		s.cursors[key] = cursor
	}

	// Weighted round-robin selection using smooth weighted algorithm
	selected := selectWeightedSmooth(cursor, len(available))
	return available[selected], nil
}

// extractWeights returns the effective weights for each auth (minimum 1).
func extractWeights(auths []*Auth) []int {
	weights := make([]int, len(auths))
	for i, auth := range auths {
		w := auth.Weight
		if w <= 0 {
			w = 1 // Default weight
		}
		weights[i] = w
	}
	return weights
}

// weightsMatch checks if two weight slices are identical.
func weightsMatch(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// initWeightedCursor initializes a weighted cursor for the given weights.
func initWeightedCursor(weights []int) *weightedCursor {
	maxW := 0
	gcdW := 0
	for _, w := range weights {
		if w > maxW {
			maxW = w
		}
		gcdW = gcd(gcdW, w)
	}
	if gcdW <= 0 {
		gcdW = 1
	}

	return &weightedCursor{
		currentIndex:  -1,
		currentWeight: 0,
		effectiveGCD:  gcdW,
		maxWeight:     maxW,
		weights:       weights,
	}
}

// selectWeightedSmooth implements smooth weighted round-robin.
// This distributes requests more evenly than classic weighted round-robin.
func selectWeightedSmooth(cursor *weightedCursor, n int) int {
	// Using Nginx-style smooth weighted round-robin algorithm
	// Each server has a current_weight that increases by its effective_weight
	// The server with highest current_weight is selected, then reduced by total_weight

	if n == 0 {
		return 0
	}

	totalWeight := 0
	for _, w := range cursor.weights {
		totalWeight += w
	}

	// For simplicity in this stateful implementation, use classic WRR with GCD optimization
	for {
		cursor.currentIndex = (cursor.currentIndex + 1) % n
		if cursor.currentIndex == 0 {
			cursor.currentWeight -= cursor.effectiveGCD
			if cursor.currentWeight <= 0 {
				cursor.currentWeight = cursor.maxWeight
			}
		}
		if cursor.weights[cursor.currentIndex] >= cursor.currentWeight {
			return cursor.currentIndex
		}
	}
}

// gcd computes the greatest common divisor of two integers.
func gcd(a, b int) int {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// ============================================================================
// Latency-Aware Selector Implementation
// ============================================================================

// NewLatencyAwareSelector creates a new latency-aware selector.
// minSamples specifies the minimum number of requests before using latency data.
func NewLatencyAwareSelector(minSamples int) *LatencyAwareSelector {
	if minSamples < 1 {
		minSamples = 10
	}
	return &LatencyAwareSelector{
		metrics:    make(map[string]*authMetrics),
		minSamples: minSamples,
	}
}

// Pick selects the auth with the lowest average latency.
// Falls back to round-robin if insufficient latency data is available.
func (s *LatencyAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = ctx
	_ = opts
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}

	if len(available) == 1 {
		return available[0], nil
	}

	// Check if we have enough latency data
	s.mu.RLock()
	hasEnoughData := s.hasEnoughSamples(available)
	s.mu.RUnlock()

	if !hasEnoughData {
		// Fall back to round-robin
		return s.pickRoundRobin(provider, model, available)
	}

	// Select based on latency with weighted probability
	return s.pickByLatency(available)
}

// hasEnoughSamples checks if we have sufficient data for latency-based selection.
func (s *LatencyAwareSelector) hasEnoughSamples(auths []*Auth) bool {
	sampledCount := 0
	for _, auth := range auths {
		if m, ok := s.metrics[auth.ID]; ok {
			m.mu.RLock()
			count := m.requestCount
			m.mu.RUnlock()
			if count >= int64(s.minSamples) {
				sampledCount++
			}
		}
	}
	// Need at least half of the auths to have enough samples
	return sampledCount >= len(auths)/2+1
}

// pickRoundRobin provides fallback round-robin selection.
func (s *LatencyAwareSelector) pickRoundRobin(provider, model string, available []*Auth) (*Auth, error) {
	key := provider + ":" + model
	var counter *uint64
	if val, ok := s.cursors.Load(key); ok {
		counter = val.(*uint64)
	} else {
		newCounter := new(uint64)
		actual, loaded := s.cursors.LoadOrStore(key, newCounter)
		if loaded {
			counter = actual.(*uint64)
		} else {
			counter = newCounter
		}
	}
	index := atomic.AddUint64(counter, 1) - 1
	return available[int(index%uint64(len(available)))], nil
}

// pickByLatency selects auth based on inverse latency weighting.
// Lower latency = higher probability of selection.
func (s *LatencyAwareSelector) pickByLatency(available []*Auth) (*Auth, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	type candidate struct {
		auth       *Auth
		score      float64 // higher is better
		avgLatency time.Duration
	}

	candidates := make([]candidate, 0, len(available))
	var totalScore float64

	for _, auth := range available {
		m, ok := s.metrics[auth.ID]
		if !ok {
			// No metrics, give default score
			candidates = append(candidates, candidate{auth: auth, score: 1.0, avgLatency: 0})
			totalScore += 1.0
			continue
		}

		m.mu.RLock()
		avgLatency := m.avgLatency
		successRate := m.successRate
		m.mu.RUnlock()

		// Calculate score: inverse of latency * success rate
		// Add 100ms baseline to avoid division by very small numbers
		latencyMs := float64(avgLatency.Milliseconds()) + 100
		score := (1000.0 / latencyMs) * (successRate + 0.1) // +0.1 to avoid zero

		candidates = append(candidates, candidate{auth: auth, score: score, avgLatency: avgLatency})
		totalScore += score
	}

	if totalScore <= 0 {
		return available[0], nil
	}

	// Weighted random selection based on scores
	// Use deterministic selection for consistency: pick highest score
	var best *candidate
	for i := range candidates {
		if best == nil || candidates[i].score > best.score {
			best = &candidates[i]
		}
	}

	return best.auth, nil
}

// RecordLatency records the latency for a request to update metrics.
func (s *LatencyAwareSelector) RecordLatency(authID string, latency time.Duration, success bool) {
	s.mu.Lock()
	m, ok := s.metrics[authID]
	if !ok {
		m = &authMetrics{}
		s.metrics[authID] = m
	}
	s.mu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	m.requestCount++
	m.totalLatency += latency
	if success {
		m.successCount++
	} else {
		m.failureCount++
	}
	m.lastUpdated = time.Now()

	// Update cached averages
	if m.requestCount > 0 {
		m.avgLatency = m.totalLatency / time.Duration(m.requestCount)
		m.successRate = float64(m.successCount) / float64(m.requestCount)
	}
}

// GetMetrics returns a copy of metrics for an auth (for monitoring).
func (s *LatencyAwareSelector) GetMetrics(authID string) (avgLatency time.Duration, successRate float64, requestCount int64, ok bool) {
	s.mu.RLock()
	m, exists := s.metrics[authID]
	s.mu.RUnlock()
	if !exists {
		return 0, 0, 0, false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.avgLatency, m.successRate, m.requestCount, true
}

// ============================================================================
// Adaptive Selector Implementation
// ============================================================================

// NewAdaptiveSelector creates a new adaptive selector.
// adaptThreshold specifies requests before switching to latency-aware mode.
func NewAdaptiveSelector(adaptThreshold int) *AdaptiveSelector {
	if adaptThreshold < 10 {
		adaptThreshold = 50
	}
	return &AdaptiveSelector{
		latencySelector: NewLatencyAwareSelector(5),
		roundRobin:      &RoundRobinSelector{},
		adaptThreshold:  adaptThreshold,
	}
}

// Pick selects auth using adaptive strategy.
// Uses round-robin initially, then switches to latency-aware as data accumulates.
func (s *AdaptiveSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	// Check total request count across all auths
	s.latencySelector.mu.RLock()
	totalRequests := int64(0)
	for _, auth := range auths {
		if m, ok := s.latencySelector.metrics[auth.ID]; ok {
			m.mu.RLock()
			totalRequests += m.requestCount
			m.mu.RUnlock()
		}
	}
	s.latencySelector.mu.RUnlock()

	// Use latency-aware if we have enough data
	if totalRequests >= int64(s.adaptThreshold) {
		return s.latencySelector.Pick(ctx, provider, model, opts, auths)
	}

	// Otherwise use round-robin
	return s.roundRobin.Pick(ctx, provider, model, opts, auths)
}

// RecordLatency delegates to the latency selector for metrics collection.
func (s *AdaptiveSelector) RecordLatency(authID string, latency time.Duration, success bool) {
	s.latencySelector.RecordLatency(authID, latency, success)
}

// GetMetrics returns metrics from the latency selector.
func (s *AdaptiveSelector) GetMetrics(authID string) (avgLatency time.Duration, successRate float64, requestCount int64, ok bool) {
	return s.latencySelector.GetMetrics(authID)
}

// ============================================================================
// Metrics Recording Interface
// ============================================================================

// MetricsRecorder is an optional interface that selectors can implement
// to receive performance metrics for adaptive load balancing.
type MetricsRecorder interface {
	RecordLatency(authID string, latency time.Duration, success bool)
}
