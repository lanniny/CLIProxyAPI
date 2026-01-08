package auth

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	defaultHealthCheckInterval = 5 * time.Minute
	defaultHealthCheckTimeout  = 30 * time.Second
	minHealthCheckInterval     = 30 * time.Second
)

// HealthChecker performs periodic health checks on credentials.
type HealthChecker struct {
	manager  *Manager
	interval time.Duration
	timeout  time.Duration

	mu       sync.Mutex
	running  atomic.Bool
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// HealthCheckConfig holds configuration for the health checker.
type HealthCheckConfig struct {
	Enabled  bool
	Interval time.Duration
	Timeout  time.Duration
}

// NewHealthChecker creates a new health checker for the given manager.
func NewHealthChecker(manager *Manager, cfg HealthCheckConfig) *HealthChecker {
	interval := cfg.Interval
	if interval < minHealthCheckInterval {
		interval = defaultHealthCheckInterval
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultHealthCheckTimeout
	}

	return &HealthChecker{
		manager:  manager,
		interval: interval,
		timeout:  timeout,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

// Start begins the background health check loop.
func (h *HealthChecker) Start(ctx context.Context) {
	if !h.running.CompareAndSwap(false, true) {
		return // Already running
	}

	go h.run(ctx)
}

// Stop gracefully stops the health checker.
func (h *HealthChecker) Stop() {
	if !h.running.CompareAndSwap(true, false) {
		return // Not running
	}

	h.mu.Lock()
	close(h.stopCh)
	h.mu.Unlock()

	<-h.doneCh
}

func (h *HealthChecker) run(ctx context.Context) {
	defer close(h.doneCh)

	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()

	// Run initial check after a short delay
	initialDelay := time.After(10 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return
		case <-h.stopCh:
			return
		case <-initialDelay:
			h.checkAll(ctx)
		case <-ticker.C:
			h.checkAll(ctx)
		}
	}
}

// checkAll performs health checks on all registered credentials.
func (h *HealthChecker) checkAll(ctx context.Context) {
	if h.manager == nil {
		return
	}

	auths := h.manager.List()
	if len(auths) == 0 {
		return
	}

	now := time.Now()
	var wg sync.WaitGroup

	for _, auth := range auths {
		if auth.Disabled {
			continue
		}

		// Skip if recently successful (within last interval)
		if !auth.UpdatedAt.IsZero() && now.Sub(auth.UpdatedAt) < h.interval/2 {
			continue
		}

		// Skip if in cooldown
		if auth.Unavailable && auth.NextRetryAfter.After(now) {
			continue
		}

		wg.Add(1)
		go func(a *Auth) {
			defer wg.Done()
			h.checkAuth(ctx, a)
		}(auth)
	}

	wg.Wait()
}

// checkAuth performs a health check on a single credential.
func (h *HealthChecker) checkAuth(ctx context.Context, auth *Auth) {
	if auth == nil || h.manager == nil {
		return
	}

	checkCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	h.manager.mu.RLock()
	executor, ok := h.manager.executors[auth.Provider]
	h.manager.mu.RUnlock()

	if !ok {
		return
	}

	now := time.Now()
	var checkErr error

	// Check if executor implements HealthCheckExecutor for custom health checks
	// This is useful for API key credentials that cannot be validated via Refresh
	if hc, ok := executor.(HealthCheckExecutor); ok {
		checkErr = hc.HealthCheck(checkCtx, auth)
	} else {
		// Fall back to Refresh for OAuth credentials
		_, checkErr = executor.Refresh(checkCtx, auth)
	}

	if checkErr != nil {
		log.WithError(checkErr).WithFields(log.Fields{
			"auth_id":  auth.ID,
			"provider": auth.Provider,
		}).Debug("Health check failed for credential")

		// Mark as unavailable with short cooldown and update health check stats
		h.manager.mu.Lock()
		var authCopy *Auth
		if current, exists := h.manager.auths[auth.ID]; exists {
			current.Unavailable = true
			current.NextRetryAfter = now.Add(time.Minute)
			current.StatusMessage = "health check failed"
			current.LastHealthCheck = now
			current.HealthCheckCount++
			current.HealthCheckFails++
			current.LastHealthError = checkErr.Error()
			authCopy = current.Clone()
		}
		h.manager.mu.Unlock()

		// Persist health check state
		if authCopy != nil {
			_ = h.manager.persist(ctx, authCopy)
		}

		// Notify circuit breaker of health check failure
		if h.manager.circuitBreakers != nil {
			h.manager.circuitBreakers.RecordFailure(auth.ID)
		}
		return
	}

	// Health check passed
	h.manager.mu.Lock()
	var authCopy *Auth
	if current, exists := h.manager.auths[auth.ID]; exists {
		// Clear any previous unavailability
		if current.StatusMessage == "health check failed" {
			current.Unavailable = false
			current.StatusMessage = ""
			current.NextRetryAfter = time.Time{}
		}
		// Mark as verified on first successful health check
		current.Verified = true
		current.UpdatedAt = now
		current.LastHealthCheck = now
		current.HealthCheckCount++
		current.HealthCheckFails = 0 // Reset consecutive failures
		current.LastHealthError = ""
		authCopy = current.Clone()
	}
	h.manager.mu.Unlock()

	// Persist health check state
	if authCopy != nil {
		_ = h.manager.persist(ctx, authCopy)
	}

	// Notify circuit breaker of health check success
	if h.manager.circuitBreakers != nil {
		h.manager.circuitBreakers.RecordSuccess(auth.ID)
	}

	log.WithFields(log.Fields{
		"auth_id":  auth.ID,
		"provider": auth.Provider,
	}).Debug("Health check passed for credential")
}

// ParseHealthCheckConfig parses health check configuration from string values.
func ParseHealthCheckConfig(enabled bool, intervalStr, timeoutStr string) HealthCheckConfig {
	cfg := HealthCheckConfig{
		Enabled:  enabled,
		Interval: defaultHealthCheckInterval,
		Timeout:  defaultHealthCheckTimeout,
	}

	if intervalStr != "" {
		if d, err := time.ParseDuration(intervalStr); err == nil && d > 0 {
			cfg.Interval = d
		}
	}

	if timeoutStr != "" {
		if d, err := time.ParseDuration(timeoutStr); err == nil && d > 0 {
			cfg.Timeout = d
		}
	}

	return cfg
}

// Running returns whether the health checker is currently running.
func (h *HealthChecker) Running() bool {
	return h.running.Load()
}
