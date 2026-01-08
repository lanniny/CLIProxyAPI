package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// mockProviderExecutor is a mock implementation of ProviderExecutor for testing.
type mockProviderExecutor struct {
	identifier      string
	executeFn       func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	executeStreamFn func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (<-chan cliproxyexecutor.StreamChunk, error)
	refreshFn       func(ctx context.Context, auth *Auth) (*Auth, error)
	countTokensFn   func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
}

func (m *mockProviderExecutor) Identifier() string {
	return m.identifier
}

func (m *mockProviderExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if m.executeFn != nil {
		return m.executeFn(ctx, auth, req, opts)
	}
	return cliproxyexecutor.Response{}, nil
}

func (m *mockProviderExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (<-chan cliproxyexecutor.StreamChunk, error) {
	if m.executeStreamFn != nil {
		return m.executeStreamFn(ctx, auth, req, opts)
	}
	return nil, nil
}

func (m *mockProviderExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	if m.refreshFn != nil {
		return m.refreshFn(ctx, auth)
	}
	return auth, nil
}

func (m *mockProviderExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if m.countTokensFn != nil {
		return m.countTokensFn(ctx, auth, req, opts)
	}
	return cliproxyexecutor.Response{}, nil
}

// mockHook is a mock implementation of Hook for testing.
type mockHook struct {
	onAuthRegisteredFn func(ctx context.Context, auth *Auth)
	onAuthUpdatedFn    func(ctx context.Context, auth *Auth)
	onResultFn         func(ctx context.Context, result Result)
}

func (m *mockHook) OnAuthRegistered(ctx context.Context, auth *Auth) {
	if m.onAuthRegisteredFn != nil {
		m.onAuthRegisteredFn(ctx, auth)
	}
}

func (m *mockHook) OnAuthUpdated(ctx context.Context, auth *Auth) {
	if m.onAuthUpdatedFn != nil {
		m.onAuthUpdatedFn(ctx, auth)
	}
}

func (m *mockHook) OnResult(ctx context.Context, result Result) {
	if m.onResultFn != nil {
		m.onResultFn(ctx, result)
	}
}

// mockRegistry is a mock implementation of ModelRegistry for testing.
type mockRegistry struct {
	clientSupportsModelFn      func(clientID, model string) bool
	setModelQuotaExceededFn    func(clientID, model string)
	clearModelQuotaExceededFn  func(clientID, model string)
	suspendClientModelFn       func(clientID, model, reason string)
	resumeClientModelFn        func(clientID, model string)
}

func (m *mockRegistry) ClientSupportsModel(clientID, model string) bool {
	if m.clientSupportsModelFn != nil {
		return m.clientSupportsModelFn(clientID, model)
	}
	return true
}

func (m *mockRegistry) SetModelQuotaExceeded(clientID, model string) {
	if m.setModelQuotaExceededFn != nil {
		m.setModelQuotaExceededFn(clientID, model)
	}
}

func (m *mockRegistry) ClearModelQuotaExceeded(clientID, model string) {
	if m.clearModelQuotaExceededFn != nil {
		m.clearModelQuotaExceededFn(clientID, model)
	}
}

func (m *mockRegistry) SuspendClientModel(clientID, model, reason string) {
	if m.suspendClientModelFn != nil {
		m.suspendClientModelFn(clientID, model, reason)
	}
}

func (m *mockRegistry) ResumeClientModel(clientID, model string) {
	if m.resumeClientModelFn != nil {
		m.resumeClientModelFn(clientID, model)
	}
}

func TestCircuitBreakerIntegration(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil, nil)
	executor := &mockProviderExecutor{
		identifier: "test",
		executeFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			return cliproxyexecutor.Response{}, nil
		},
	}
	manager.RegisterExecutor(executor)

	// Register a test auth
	auth := &Auth{
		ID:       "test-auth",
		Provider: "test",
		Status:   StatusActive,
	}
	_, _ = manager.Register(context.Background(), auth)

	// Test that circuit breaker starts in closed state
	breaker := manager.circuitBreakers.GetBreaker("test-auth")
	if breaker.State() != CircuitClosed {
		t.Errorf("expected circuit breaker to be closed initially, got %v", breaker.State())
	}

	// Simulate failures to open the circuit
	// Must call Allow() first to register the request, then RecordFailure()
	for i := 0; i < 5; i++ {
		if !manager.circuitBreakers.Allow("test-auth") {
			t.Fatalf("expected request %d to be allowed", i)
		}
		manager.circuitBreakers.RecordFailure("test-auth")
	}

	// Verify circuit is now open
	if breaker.State() != CircuitOpen {
		t.Errorf("expected circuit to be open after failures, got %v", breaker.State())
	}

	// Test that Allow returns false for open circuit
	if manager.circuitBreakers.Allow("test-auth") {
		t.Error("expected Allow() to return false for open circuit")
	}

	// Verify circuit stays open (blocking requests)
	// After waiting for open timeout, Allow() would transition to half-open
	// For this test, we just verify the circuit is properly open
	_ = breaker
}

func TestLatencyRecording(t *testing.T) {
	t.Parallel()

	// Create a latency-aware selector for tracking
	latencySelector := NewLatencyAwareSelector(5)

	var recordedLatencies []struct {
		authID  string
		latency time.Duration
		success bool
	}
	var mu sync.Mutex

	// Create a selector that captures latency
	trackingSelector := &trackingSelector{
		LatencyAwareSelector: latencySelector,
		recordings:           &recordedLatencies,
		recordingsMu:         &mu,
	}

	manager := NewManager(nil, trackingSelector, nil, nil)

	executor := &mockProviderExecutor{
		identifier: "test",
		executeFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			return cliproxyexecutor.Response{}, nil
		},
	}
	manager.RegisterExecutor(executor)

	// Register test auths
	for i := 0; i < 5; i++ {
		auth := &Auth{
			ID:       string(rune('a' + i)),
			Provider: "test",
			Status:   StatusActive,
		}
		_, _ = manager.Register(context.Background(), auth)
	}

	// Test recordLatencyIfSupported
	manager.recordLatencyIfSupported("a", 100*time.Millisecond, true)
	manager.recordLatencyIfSupported("a", 200*time.Millisecond, true)
	manager.recordLatencyIfSupported("b", 500*time.Millisecond, false)

	// Verify latency was recorded
	avgLatency, successRate, count, ok := trackingSelector.GetMetrics("a")
	if !ok {
		t.Fatal("expected metrics for auth 'a'")
	}
	if count != 2 {
		t.Errorf("expected 2 requests, got %d", count)
	}
	if avgLatency != 150*time.Millisecond {
		t.Errorf("expected avg latency 150ms, got %v", avgLatency)
	}
	if successRate != 1.0 {
		t.Errorf("expected success rate 1.0, got %f", successRate)
	}

	// Verify failure was recorded for auth 'b'
	_, successRateB, countB, ok := trackingSelector.GetMetrics("b")
	if !ok {
		t.Fatal("expected metrics for auth 'b'")
	}
	if countB != 1 {
		t.Errorf("expected 1 request for auth 'b', got %d", countB)
	}
	if successRateB != 0.0 {
		t.Errorf("expected success rate 0.0 for auth 'b', got %f", successRateB)
	}

	// Test nil manager doesn't panic
	managerNil := (*Manager)(nil)
	managerNil.recordLatencyIfSupported("a", 100*time.Millisecond, true)
}

func TestHealthCheckerLifecycle(t *testing.T) {
	t.Parallel()

	// Create a manager
	manager := NewManager(nil, nil, nil, nil)

	// Create health checker config
	cfg := HealthCheckConfig{
		Enabled:  true,
		Interval: time.Second,
		Timeout:  time.Second,
	}

	// Create health checker
	healthChecker := NewHealthChecker(manager, cfg)

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

func TestCircuitBreakerIntegration_MultipleAuths(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil, nil)

	// Register multiple executors and auths
	providers := []string{"gemini", "claude", "codex"}
	for _, p := range providers {
		executor := &mockProviderExecutor{
			identifier: p,
			executeFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				return cliproxyexecutor.Response{}, nil
			},
		}
		manager.RegisterExecutor(executor)

		auth := &Auth{
			ID:       p + "-auth",
			Provider: p,
			Status:   StatusActive,
		}
		_, _ = manager.Register(context.Background(), auth)
	}

	// Open circuit for gemini-auth only
	for i := 0; i < 5; i++ {
		if !manager.circuitBreakers.Allow("gemini-auth") {
			t.Fatalf("expected request %d to be allowed", i)
		}
		manager.circuitBreakers.RecordFailure("gemini-auth")
	}

	// Verify gemini circuit is open
	geminiBreaker := manager.circuitBreakers.GetBreaker("gemini-auth")
	if geminiBreaker.State() != CircuitOpen {
		t.Errorf("expected gemini circuit to be open, got %v", geminiBreaker.State())
	}

	// Verify other circuits are still closed
	claudeBreaker := manager.circuitBreakers.GetBreaker("claude-auth")
	if claudeBreaker.State() != CircuitClosed {
		t.Errorf("expected claude circuit to be closed, got %v", claudeBreaker.State())
	}

	codexBreaker := manager.circuitBreakers.GetBreaker("codex-auth")
	if codexBreaker.State() != CircuitClosed {
		t.Errorf("expected codex circuit to be closed, got %v", codexBreaker.State())
	}
}

func TestLatencyRecording_WithNilSelector(t *testing.T) {
	t.Parallel()

	// Create manager with nil selector
	manager := NewManager(nil, nil, nil, nil)

	// This should not panic
	manager.recordLatencyIfSupported("any-auth", 100*time.Millisecond, true)

	// Verify no panic occurred
	t.Log("Latency recording with nil selector completed without panic")
}

func TestLatencyRecording_WithRoundRobinSelector(t *testing.T) {
	t.Parallel()

	// Create manager with round-robin selector (does not implement MetricsRecorder)
	manager := NewManager(nil, &RoundRobinSelector{}, nil, nil)

	// This should not panic and should silently skip recording
	manager.recordLatencyIfSupported("any-auth", 100*time.Millisecond, true)

	// Verify no panic occurred and no error
	t.Log("Latency recording with round-robin selector completed without panic")
}

// trackingSelector wraps LatencyAwareSelector to track latency recordings.
type trackingSelector struct {
	*LatencyAwareSelector
	recordings  *[]struct {
		authID  string
		latency time.Duration
		success bool
	}
	recordingsMu *sync.Mutex
}

func (s *trackingSelector) RecordLatency(authID string, latency time.Duration, success bool) {
	s.recordingsMu.Lock()
	defer s.recordingsMu.Unlock()
	*s.recordings = append(*s.recordings, struct {
		authID  string
		latency time.Duration
		success bool
	}{authID, latency, success})
	s.LatencyAwareSelector.RecordLatency(authID, latency, success)
}

func TestHealthChecker_CheckAllWithNoAuths(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil, nil)

	cfg := HealthCheckConfig{
		Enabled:  true,
		Interval: time.Second,
		Timeout:  time.Second,
	}
	healthChecker := NewHealthChecker(manager, cfg)

	// checkAll with no auths should not panic
	healthChecker.checkAll(context.Background())
	t.Log("checkAll with no auths completed without panic")
}

func TestHealthChecker_CheckAuthWithNilManager(t *testing.T) {
	t.Parallel()

	cfg := HealthCheckConfig{
		Enabled:  true,
		Interval: time.Second,
		Timeout:  time.Second,
	}
	healthChecker := NewHealthChecker(nil, cfg)

	// checkAuth with nil manager should not panic
	healthChecker.checkAuth(context.Background(), &Auth{ID: "test"})
	t.Log("checkAuth with nil manager completed without panic")
}

func TestHealthChecker_WithAuthRefresh(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil, nil)

	executor := &mockProviderExecutor{
		identifier: "test",
		refreshFn: func(ctx context.Context, auth *Auth) (*Auth, error) {
			// Simulate successful refresh
			return auth, nil
		},
	}
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "test-auth",
		Provider: "test",
		Status:   StatusActive,
	}
	_, _ = manager.Register(context.Background(), auth)

	cfg := HealthCheckConfig{
		Enabled:  true,
		Interval: time.Second,
		Timeout:  time.Second,
	}
	healthChecker := NewHealthChecker(manager, cfg)

	// checkAuth should work with a valid executor
	healthChecker.checkAuth(context.Background(), auth)
	t.Log("checkAuth with valid executor completed without panic")
}

func TestHealthChecker_WithAuthRefreshFailure(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil, nil)

	executor := &mockProviderExecutor{
		identifier: "test",
		refreshFn: func(ctx context.Context, auth *Auth) (*Auth, error) {
			// Simulate refresh failure
			return nil, errors.New("refresh failed")
		},
	}
	manager.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "test-auth",
		Provider: "test",
		Status:   StatusActive,
	}
	_, _ = manager.Register(context.Background(), auth)

	cfg := HealthCheckConfig{
		Enabled:  true,
		Interval: time.Second,
		Timeout:  time.Second,
	}
	healthChecker := NewHealthChecker(manager, cfg)

	// checkAuth should mark auth as unavailable on failure
	healthChecker.checkAuth(context.Background(), auth)

	// Verify auth was marked unavailable
	manager.mu.RLock()
	current, exists := manager.auths["test-auth"]
	manager.mu.RUnlock()

	if !exists {
		t.Fatal("expected auth to exist")
	}
	if !current.Unavailable {
		t.Error("expected auth to be marked unavailable after failed health check")
	}
}

func TestManager_RegisterWithNilAuth(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil, nil)

	// Register with nil auth should not panic
	_, err := manager.Register(context.Background(), nil)
	_ = err
	t.Log("Register with nil auth completed without panic")
}

func TestManager_RegisterExecutorNil(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil, nil)

	// Register nil executor should not panic
	manager.RegisterExecutor(nil)
	t.Log("RegisterExecutor with nil completed without panic")
}

func TestManager_List(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil, nil)

	// List with no auths should return empty
	auths := manager.List()
	if len(auths) != 0 {
		t.Errorf("expected 0 auths, got %d", len(auths))
	}

	// Register some auths
	for i := 0; i < 3; i++ {
		auth := &Auth{
			ID:       fmt.Sprintf("auth-%d", i),
			Provider: "test",
			Status:   StatusActive,
		}
		_, _ = manager.Register(context.Background(), auth)
	}

	// List should return all auths
	auths = manager.List()
	if len(auths) != 3 {
		t.Errorf("expected 3 auths, got %d", len(auths))
	}
}

func TestManager_GetByID(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil, nil)

	// Get non-existent auth should return nil
	auth, ok := manager.GetByID("nonexistent")
	if ok || auth != nil {
		t.Error("expected nil for non-existent auth")
	}

	// Register an auth
	auth = &Auth{
		ID:       "test-auth",
		Provider: "test",
		Status:   StatusActive,
	}
	_, _ = manager.Register(context.Background(), auth)

	// Get existing auth should return it
	retrieved, ok := manager.GetByID("test-auth")
	if !ok || retrieved == nil {
		t.Fatal("expected auth to be retrieved")
	}
	if retrieved.ID != "test-auth" {
		t.Errorf("expected ID 'test-auth', got '%s'", retrieved.ID)
	}
}

func TestManager_ExecutorFor(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil, nil)

	// Get non-existent executor should return nil
	executor := manager.executorFor("nonexistent")
	if executor != nil {
		t.Error("expected nil for non-existent executor")
	}

	// Register an executor
	mockExec := &mockProviderExecutor{identifier: "test"}
	manager.RegisterExecutor(mockExec)

	// Get existing executor should return it
	retrieved := manager.executorFor("test")
	if retrieved == nil {
		t.Fatal("expected executor to be retrieved")
	}
	if retrieved.Identifier() != "test" {
		t.Errorf("expected identifier 'test', got '%s'", retrieved.Identifier())
	}
}

func TestManager_Update(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil, nil)

	// Update non-existent auth should not panic (just no-op)
	_, _ = manager.Update(context.Background(), &Auth{ID: "nonexistent"})
	t.Log("Update with non-existent auth completed without panic")

	// Register an auth
	auth := &Auth{
		ID:       "test-auth",
		Provider: "test",
		Status:   StatusActive,
	}
	_, _ = manager.Register(context.Background(), auth)

	// Update the auth
	auth.Status = StatusDisabled
	_, err := manager.Update(context.Background(), auth)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// Verify auth was updated
	retrieved, ok := manager.GetByID("test-auth")
	if !ok {
		t.Fatal("expected auth to exist")
	}
	if retrieved.Status != StatusDisabled {
		t.Error("expected auth status to be updated")
	}
}

// TestGetCredentialType_OAuth verifies that getCredentialType returns "oauth"
// for credentials with Metadata["email"] set.
func TestGetCredentialType_OAuth(t *testing.T) {
	t.Parallel()
	auth := &Auth{
		ID:       "oauth-auth",
		Provider: "gemini",
		Metadata: map[string]any{"email": "test@example.com"},
	}
	got := getCredentialType(auth)
	if got != "oauth" {
		t.Errorf("getCredentialType() = %q, want %q", got, "oauth")
	}
}

// TestGetCredentialType_APIKey verifies that getCredentialType returns "api_key"
// for credentials with Attributes["api_key"] set.
func TestGetCredentialType_APIKey(t *testing.T) {
	t.Parallel()
	auth := &Auth{
		ID:         "apikey-auth",
		Provider:   "gemini",
		Attributes: map[string]string{"api_key": "sk-xxxxx"},
	}
	got := getCredentialType(auth)
	if got != "api_key" {
		t.Errorf("getCredentialType() = %q, want %q", got, "api_key")
	}
}

// TestGetCredentialType_Empty verifies that getCredentialType returns ""
// for credentials with neither email nor api_key set.
func TestGetCredentialType_Empty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		auth *Auth
	}{
		{
			name: "nil auth",
			auth: nil,
		},
		{
			name: "empty auth",
			auth: &Auth{
				ID:       "empty-auth",
				Provider: "gemini",
			},
		},
		{
			name: "empty metadata and attributes",
			auth: &Auth{
				ID:         "empty-maps",
				Provider:   "gemini",
				Metadata:   map[string]any{},
				Attributes: map[string]string{},
			},
		},
		{
			name: "empty email string",
			auth: &Auth{
				ID:       "empty-email",
				Provider: "gemini",
				Metadata: map[string]any{"email": ""},
			},
		},
		{
			name: "whitespace email",
			auth: &Auth{
				ID:       "whitespace-email",
				Provider: "gemini",
				Metadata: map[string]any{"email": "   "},
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := getCredentialType(tc.auth)
			if got != "" {
				t.Errorf("getCredentialType() = %q, want empty string", got)
			}
		})
	}
}

// TestPickNext_OAuthFirst verifies that OAuth credentials are selected first
// when both OAuth and API key credentials are available.
func TestPickNext_OAuthFirst(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil, nil)
	executor := &mockProviderExecutor{
		identifier: "test",
		executeFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			return cliproxyexecutor.Response{}, nil
		},
	}
	manager.RegisterExecutor(executor)

	// Register OAuth credential
	oauthAuth := &Auth{
		ID:       "oauth-cred",
		Provider: "test",
		Status:   StatusActive,
		Metadata: map[string]any{"email": "oauth@example.com"},
	}
	_, _ = manager.Register(context.Background(), oauthAuth)

	// Register API key credential
	apiKeyAuth := &Auth{
		ID:         "apikey-cred",
		Provider:   "test",
		Status:     StatusActive,
		Attributes: map[string]string{"api_key": "sk-test-key"},
	}
	_, _ = manager.Register(context.Background(), apiKeyAuth)

	// pickNext should select OAuth credential first
	tried := make(map[string]struct{})
	selected, _, err := manager.pickNext(context.Background(), "test", "", cliproxyexecutor.Options{}, tried)
	if err != nil {
		t.Fatalf("pickNext() error = %v", err)
	}
	if selected == nil {
		t.Fatal("pickNext() returned nil auth")
	}

	// Verify OAuth credential was selected
	credType := getCredentialType(selected)
	if credType != "oauth" {
		t.Errorf("expected OAuth credential to be selected first, got type %q (ID: %s)", credType, selected.ID)
	}
	if selected.ID != "oauth-cred" {
		t.Errorf("expected oauth-cred to be selected, got %s", selected.ID)
	}
}

// TestPickNext_FallbackToAPIKey verifies that API key credentials are used
// when all OAuth credentials have been tried (exhausted).
func TestPickNext_FallbackToAPIKey(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil, nil)
	executor := &mockProviderExecutor{
		identifier: "test",
		executeFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			return cliproxyexecutor.Response{}, nil
		},
	}
	manager.RegisterExecutor(executor)

	// Register OAuth credential
	oauthAuth := &Auth{
		ID:       "oauth-cred",
		Provider: "test",
		Status:   StatusActive,
		Metadata: map[string]any{"email": "oauth@example.com"},
	}
	_, _ = manager.Register(context.Background(), oauthAuth)

	// Register API key credential
	apiKeyAuth := &Auth{
		ID:         "apikey-cred",
		Provider:   "test",
		Status:     StatusActive,
		Attributes: map[string]string{"api_key": "sk-test-key"},
	}
	_, _ = manager.Register(context.Background(), apiKeyAuth)

	// Mark OAuth credential as tried (exhausted)
	tried := make(map[string]struct{})
	tried["oauth-cred"] = struct{}{}

	// pickNext should now fall back to API key credential
	selected, _, err := manager.pickNext(context.Background(), "test", "", cliproxyexecutor.Options{}, tried)
	if err != nil {
		t.Fatalf("pickNext() error = %v", err)
	}
	if selected == nil {
		t.Fatal("pickNext() returned nil auth")
	}

	// Verify API key credential was selected as fallback
	credType := getCredentialType(selected)
	if credType != "api_key" {
		t.Errorf("expected API key credential as fallback, got type %q (ID: %s)", credType, selected.ID)
	}
	if selected.ID != "apikey-cred" {
		t.Errorf("expected apikey-cred to be selected, got %s", selected.ID)
	}
}

// TestPickNext_NoFallbackWhenOAuthAvailable verifies that API key credentials
// are NOT selected when OAuth credentials are still available (not in tried map).
func TestPickNext_NoFallbackWhenOAuthAvailable(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil, nil)
	executor := &mockProviderExecutor{
		identifier: "test",
		executeFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			return cliproxyexecutor.Response{}, nil
		},
	}
	manager.RegisterExecutor(executor)

	// Register multiple OAuth credentials
	oauthAuth1 := &Auth{
		ID:       "oauth-cred-1",
		Provider: "test",
		Status:   StatusActive,
		Metadata: map[string]any{"email": "oauth1@example.com"},
	}
	_, _ = manager.Register(context.Background(), oauthAuth1)

	oauthAuth2 := &Auth{
		ID:       "oauth-cred-2",
		Provider: "test",
		Status:   StatusActive,
		Metadata: map[string]any{"email": "oauth2@example.com"},
	}
	_, _ = manager.Register(context.Background(), oauthAuth2)

	// Register API key credential
	apiKeyAuth := &Auth{
		ID:         "apikey-cred",
		Provider:   "test",
		Status:     StatusActive,
		Attributes: map[string]string{"api_key": "sk-test-key"},
	}
	_, _ = manager.Register(context.Background(), apiKeyAuth)

	// Mark only one OAuth credential as tried
	tried := make(map[string]struct{})
	tried["oauth-cred-1"] = struct{}{}

	// pickNext should select the remaining OAuth credential, NOT the API key
	selected, _, err := manager.pickNext(context.Background(), "test", "", cliproxyexecutor.Options{}, tried)
	if err != nil {
		t.Fatalf("pickNext() error = %v", err)
	}
	if selected == nil {
		t.Fatal("pickNext() returned nil auth")
	}

	// Verify OAuth credential was selected (not API key)
	credType := getCredentialType(selected)
	if credType != "oauth" {
		t.Errorf("expected remaining OAuth credential to be selected, got type %q (ID: %s)", credType, selected.ID)
	}
	if selected.ID != "oauth-cred-2" {
		t.Errorf("expected oauth-cred-2 to be selected, got %s", selected.ID)
	}
}

// =============================================================================
// Integration Tests for executeWithProvider() - End-to-End Fallback Scenarios
// =============================================================================
