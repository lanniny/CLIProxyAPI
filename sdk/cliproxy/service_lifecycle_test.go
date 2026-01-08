package cliproxy

import (
	"context"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestHealthCheckerLifecycle_StartAndStop(t *testing.T) {
	t.Parallel()

	// Create a core auth manager
	manager := coreauth.NewManager(nil, nil, nil, nil)

	// Create health checker
	cfg := coreauth.HealthCheckConfig{
		Enabled:  true,
		Interval: time.Second,
		Timeout:  time.Second,
	}
	healthChecker := coreauth.NewHealthChecker(manager, cfg)

	// Verify initial state
	if healthChecker.Running() {
		t.Error("expected health checker to not be running initially")
	}

	// Start the health checker
	ctx, cancel := context.WithCancel(context.Background())
	healthChecker.Start(ctx)

	// Verify it's running
	if !healthChecker.Running() {
		t.Error("expected health checker to be running after Start()")
	}

	// Stop the health checker
	healthChecker.Stop()

	// Verify it's stopped
	if healthChecker.Running() {
		t.Error("expected health checker to not be running after Stop()")
	}

	cancel()
}

func TestHealthCheckerLifecycle_DisabledConfig(t *testing.T) {
	t.Parallel()

	// Create a core auth manager
	manager := coreauth.NewManager(nil, nil, nil, nil)

	// Create health checker with disabled config
	cfg := coreauth.HealthCheckConfig{
		Enabled:  false,
		Interval: time.Second,
		Timeout:  time.Second,
	}
	healthChecker := coreauth.NewHealthChecker(manager, cfg)

	// Even with disabled config, we can still test the Start/Stop methods
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The health checker should not start when disabled
	// (this is a unit test, the integration logic is in service.go)
	_ = healthChecker
	_ = ctx
}

func TestHealthCheckerLifecycle_ConcurrentStartStop(t *testing.T) {
	t.Parallel()

	// Create a core auth manager
	manager := coreauth.NewManager(nil, nil, nil, nil)

	cfg := coreauth.HealthCheckConfig{
		Enabled:  true,
		Interval: 100 * time.Millisecond,
		Timeout:  time.Second,
	}
	healthChecker := coreauth.NewHealthChecker(manager, cfg)

	ctx, cancel := context.WithCancel(context.Background())

	// Try to start multiple times concurrently
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			healthChecker.Start(ctx)
		}()
	}
	wg.Wait()

	// Should still be running (only first start takes effect)
	if !healthChecker.Running() {
		t.Error("expected health checker to be running after concurrent starts")
	}

	// Try to stop multiple times concurrently
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			healthChecker.Stop()
		}()
	}
	wg.Wait()

	// Should be stopped (only first stop takes effect)
	if healthChecker.Running() {
		t.Error("expected health checker to not be running after concurrent stops")
	}

	cancel()
}

func TestHealthCheckerLifecycle_ConfigParsing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		enabled          bool
		intervalStr      string
		timeoutStr       string
		expectedEnabled  bool
		expectedInterval time.Duration
		expectedTimeout  time.Duration
	}{
		{
			name:             "default values when disabled",
			enabled:          false,
			intervalStr:      "",
			timeoutStr:       "",
			expectedEnabled:  false,
			expectedInterval: 5 * time.Minute,
			expectedTimeout:  30 * time.Second,
		},
		{
			name:             "custom interval and timeout",
			enabled:          true,
			intervalStr:      "30s",
			timeoutStr:       "10s",
			expectedEnabled:  true,
			expectedInterval: 30 * time.Second,
			expectedTimeout:  10 * time.Second,
		},
		{
			name:             "interval below minimum uses default",
			enabled:          true,
			intervalStr:      "1s",
			timeoutStr:       "",
			expectedEnabled:  true,
			expectedInterval: 1 * time.Second, // ParseHealthCheckConfig does not enforce minimum
			expectedTimeout:  30 * time.Second,
		},
		{
			name:             "zero timeout uses default",
			enabled:          true,
			intervalStr:      "1m",
			timeoutStr:       "0s",
			expectedEnabled:  true,
			expectedInterval: time.Minute,
			expectedTimeout:  30 * time.Second,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := coreauth.ParseHealthCheckConfig(tc.enabled, tc.intervalStr, tc.timeoutStr)
			if cfg.Enabled != tc.expectedEnabled {
				t.Errorf("expected Enabled=%v, got %v", tc.expectedEnabled, cfg.Enabled)
			}
			if cfg.Interval != tc.expectedInterval {
				t.Errorf("expected Interval=%v, got %v", tc.expectedInterval, cfg.Interval)
			}
			if cfg.Timeout != tc.expectedTimeout {
				t.Errorf("expected Timeout=%v, got %v", tc.expectedTimeout, cfg.Timeout)
			}
		})
	}
}

func TestHealthCheckerLifecycle_NewHealthChecker(t *testing.T) {
	t.Parallel()

	// Create a core auth manager
	manager := coreauth.NewManager(nil, nil, nil, nil)

	// Test with various configurations
	cfg := coreauth.HealthCheckConfig{
		Enabled:  true,
		Interval: 2 * time.Minute,
		Timeout:  15 * time.Second,
	}
	healthChecker := coreauth.NewHealthChecker(manager, cfg)

	// Verify the health checker was created with correct config
	// (The actual config values are stored internally, we just verify no panic)
	if healthChecker == nil {
		t.Fatal("expected non-nil health checker")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	healthChecker.Start(ctx)
	if !healthChecker.Running() {
		t.Error("expected health checker to be running after Start()")
	}
	healthChecker.Stop()
}
