package auth

import (
	"sync"
	"sync/atomic"
	"time"
)

// CircuitState represents the state of a circuit breaker.
type CircuitState int32

const (
	// CircuitClosed allows requests to pass through normally.
	CircuitClosed CircuitState = iota
	// CircuitOpen blocks all requests.
	CircuitOpen
	// CircuitHalfOpen allows limited requests to test recovery.
	CircuitHalfOpen
)

func (s CircuitState) String() string {
	switch s {
	case CircuitClosed:
		return "closed"
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// CircuitBreakerConfig holds configuration for a circuit breaker.
type CircuitBreakerConfig struct {
	// FailureThreshold is the number of consecutive failures before opening.
	FailureThreshold int
	// SuccessThreshold is the number of consecutive successes in half-open to close.
	SuccessThreshold int
	// OpenTimeout is how long to wait before transitioning from open to half-open.
	OpenTimeout time.Duration
	// HalfOpenMaxRequests is the max concurrent requests allowed in half-open state.
	HalfOpenMaxRequests int
}

// DefaultCircuitBreakerConfig returns sensible defaults.
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		FailureThreshold:    5,
		SuccessThreshold:    3,
		OpenTimeout:         30 * time.Second,
		HalfOpenMaxRequests: 1,
	}
}

// CircuitBreaker implements the circuit breaker pattern for fault tolerance.
type CircuitBreaker struct {
	config CircuitBreakerConfig

	state              atomic.Int32
	consecutiveFailures atomic.Int32
	consecutiveSuccesses atomic.Int32
	lastFailureTime    atomic.Int64 // Unix nano
	halfOpenRequests   atomic.Int32

	mu sync.RWMutex
}

// NewCircuitBreaker creates a new circuit breaker with the given config.
func NewCircuitBreaker(config CircuitBreakerConfig) *CircuitBreaker {
	if config.FailureThreshold < 1 {
		config.FailureThreshold = 5
	}
	if config.SuccessThreshold < 1 {
		config.SuccessThreshold = 3
	}
	if config.OpenTimeout < time.Second {
		config.OpenTimeout = 30 * time.Second
	}
	if config.HalfOpenMaxRequests < 1 {
		config.HalfOpenMaxRequests = 1
	}

	cb := &CircuitBreaker{config: config}
	cb.state.Store(int32(CircuitClosed))
	return cb
}

// State returns the current circuit state.
func (cb *CircuitBreaker) State() CircuitState {
	return CircuitState(cb.state.Load())
}

// Allow checks if a request should be allowed through.
// Returns true if allowed, false if the circuit is open.
func (cb *CircuitBreaker) Allow() bool {
	state := CircuitState(cb.state.Load())

	switch state {
	case CircuitClosed:
		return true

	case CircuitOpen:
		// Check if we should transition to half-open
		lastFailureNano := cb.lastFailureTime.Load()
		if lastFailureNano == 0 {
			// No failure recorded yet, shouldn't be in open state
			cb.state.CompareAndSwap(int32(CircuitOpen), int32(CircuitClosed))
			return true
		}
		lastFailure := time.Unix(0, lastFailureNano)
		if time.Since(lastFailure) >= cb.config.OpenTimeout {
			// Try to transition to half-open
			if cb.state.CompareAndSwap(int32(CircuitOpen), int32(CircuitHalfOpen)) {
				cb.halfOpenRequests.Store(0)
				cb.consecutiveSuccesses.Store(0)
			}
			// Re-check state after potential transition
			return cb.Allow()
		}
		return false

	case CircuitHalfOpen:
		// Allow limited requests in half-open state
		current := cb.halfOpenRequests.Add(1)
		if current <= int32(cb.config.HalfOpenMaxRequests) {
			return true
		}
		cb.halfOpenRequests.Add(-1)
		return false

	default:
		return true
	}
}

// RecordSuccess records a successful request.
func (cb *CircuitBreaker) RecordSuccess() {
	state := CircuitState(cb.state.Load())

	switch state {
	case CircuitClosed:
		cb.consecutiveFailures.Store(0)

	case CircuitHalfOpen:
		successes := cb.consecutiveSuccesses.Add(1)
		if successes >= int32(cb.config.SuccessThreshold) {
			// Transition to closed
			if cb.state.CompareAndSwap(int32(CircuitHalfOpen), int32(CircuitClosed)) {
				cb.consecutiveFailures.Store(0)
				cb.consecutiveSuccesses.Store(0)
			}
		}
	}
}

// RecordFailure records a failed request.
func (cb *CircuitBreaker) RecordFailure() {
	cb.lastFailureTime.Store(time.Now().UnixNano())
	state := CircuitState(cb.state.Load())

	switch state {
	case CircuitClosed:
		failures := cb.consecutiveFailures.Add(1)
		if failures >= int32(cb.config.FailureThreshold) {
			// Transition to open
			cb.state.CompareAndSwap(int32(CircuitClosed), int32(CircuitOpen))
		}

	case CircuitHalfOpen:
		// Any failure in half-open goes back to open
		if cb.state.CompareAndSwap(int32(CircuitHalfOpen), int32(CircuitOpen)) {
			cb.consecutiveSuccesses.Store(0)
		}
	}
}

// Reset resets the circuit breaker to closed state.
func (cb *CircuitBreaker) Reset() {
	cb.state.Store(int32(CircuitClosed))
	cb.consecutiveFailures.Store(0)
	cb.consecutiveSuccesses.Store(0)
	cb.halfOpenRequests.Store(0)
}

// Stats returns current circuit breaker statistics.
func (cb *CircuitBreaker) Stats() CircuitBreakerStats {
	return CircuitBreakerStats{
		State:                CircuitState(cb.state.Load()),
		ConsecutiveFailures:  int(cb.consecutiveFailures.Load()),
		ConsecutiveSuccesses: int(cb.consecutiveSuccesses.Load()),
		LastFailureTime:      time.Unix(0, cb.lastFailureTime.Load()),
	}
}

// CircuitBreakerStats holds statistics for a circuit breaker.
type CircuitBreakerStats struct {
	State                CircuitState
	ConsecutiveFailures  int
	ConsecutiveSuccesses int
	LastFailureTime      time.Time
}

// CircuitBreakerManager manages circuit breakers for multiple auth credentials.
type CircuitBreakerManager struct {
	config   CircuitBreakerConfig
	breakers sync.Map // map[string]*CircuitBreaker (authID -> breaker)
}

// NewCircuitBreakerManager creates a new circuit breaker manager.
func NewCircuitBreakerManager(config CircuitBreakerConfig) *CircuitBreakerManager {
	return &CircuitBreakerManager{config: config}
}

// GetBreaker returns the circuit breaker for an auth ID, creating one if needed.
func (m *CircuitBreakerManager) GetBreaker(authID string) *CircuitBreaker {
	if val, ok := m.breakers.Load(authID); ok {
		return val.(*CircuitBreaker)
	}

	breaker := NewCircuitBreaker(m.config)
	actual, _ := m.breakers.LoadOrStore(authID, breaker)
	return actual.(*CircuitBreaker)
}

// Allow checks if a request to the given auth should be allowed.
func (m *CircuitBreakerManager) Allow(authID string) bool {
	return m.GetBreaker(authID).Allow()
}

// RecordSuccess records a successful request for the given auth.
func (m *CircuitBreakerManager) RecordSuccess(authID string) {
	m.GetBreaker(authID).RecordSuccess()
}

// RecordFailure records a failed request for the given auth.
func (m *CircuitBreakerManager) RecordFailure(authID string) {
	m.GetBreaker(authID).RecordFailure()
}

// Reset resets the circuit breaker for the given auth.
func (m *CircuitBreakerManager) Reset(authID string) {
	if val, ok := m.breakers.Load(authID); ok {
		val.(*CircuitBreaker).Reset()
	}
}

// Stats returns statistics for all circuit breakers.
func (m *CircuitBreakerManager) Stats() map[string]CircuitBreakerStats {
	stats := make(map[string]CircuitBreakerStats)
	m.breakers.Range(func(key, value any) bool {
		authID := key.(string)
		breaker := value.(*CircuitBreaker)
		stats[authID] = breaker.Stats()
		return true
	})
	return stats
}
