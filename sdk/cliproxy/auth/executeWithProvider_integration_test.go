package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// =============================================================================
// Integration Tests for executeWithProvider() - End-to-End Fallback Scenarios
// =============================================================================

// TestExecuteWithProvider_OAuthSuccess verifies that OAuth credentials work on first try
// and no fallback to API key occurs.
func TestExecuteWithProvider_OAuthSuccess(t *testing.T) {
	t.Parallel()

	// Track which credentials were used
	var callOrder []string
	var callMu sync.Mutex

	manager := NewManager(nil, nil, nil, nil)

	// Create executor that succeeds for OAuth credential
	executor := &mockProviderExecutor{
		identifier: "test",
		executeFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			callMu.Lock()
			callOrder = append(callOrder, auth.ID)
			callMu.Unlock()
			// Always succeed
			return cliproxyexecutor.Response{Payload: []byte(`{"success":true}`)}, nil
		},
	}
	manager.RegisterExecutor(executor)

	// Register OAuth credential (should be used first and succeed)
	oauthAuth := &Auth{
		ID:       "oauth-cred",
		Provider: "test",
		Status:   StatusActive,
		Metadata: map[string]any{"email": "oauth@example.com"},
	}
	_, _ = manager.Register(context.Background(), oauthAuth)

	// Register API key credential (should NOT be used since OAuth succeeds)
	apiKeyAuth := &Auth{
		ID:         "apikey-cred",
		Provider:   "test",
		Status:     StatusActive,
		Attributes: map[string]string{"api_key": "sk-test-key"},
	}
	_, _ = manager.Register(context.Background(), apiKeyAuth)

	// Execute request
	req := cliproxyexecutor.Request{Model: "test-model"}
	opts := cliproxyexecutor.Options{}
	resp, err := manager.executeWithProvider(context.Background(), "test", req, opts)

	// Verify success
	if err != nil {
		t.Fatalf("executeWithProvider() error = %v", err)
	}
	if resp.Payload == nil {
		t.Error("expected non-nil response payload")
	}

	// Verify only OAuth credential was used
	callMu.Lock()
	defer callMu.Unlock()
	if len(callOrder) != 1 {
		t.Errorf("expected 1 call, got %d: %v", len(callOrder), callOrder)
	}
	if len(callOrder) > 0 && callOrder[0] != "oauth-cred" {
		t.Errorf("expected oauth-cred to be used first, got %s", callOrder[0])
	}
}

// TestExecuteWithProvider_OAuthFailFallbackToAPIKey verifies that when OAuth fails,
// the system falls back to API key credential which succeeds.
func TestExecuteWithProvider_OAuthFailFallbackToAPIKey(t *testing.T) {
	t.Parallel()

	// Track which credentials were used
	var callOrder []string
	var callMu sync.Mutex

	manager := NewManager(nil, nil, nil, nil)

	// Create executor that fails for OAuth but succeeds for API key
	executor := &mockProviderExecutor{
		identifier: "test",
		executeFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			callMu.Lock()
			callOrder = append(callOrder, auth.ID)
			callMu.Unlock()

			credType := getCredentialType(auth)
			if credType == "oauth" {
				// OAuth fails
				return cliproxyexecutor.Response{}, errors.New("oauth token expired")
			}
			// API key succeeds
			return cliproxyexecutor.Response{Payload: []byte(`{"success":true}`)}, nil
		},
	}
	manager.RegisterExecutor(executor)

	// Register OAuth credential (will fail)
	oauthAuth := &Auth{
		ID:       "oauth-cred",
		Provider: "test",
		Status:   StatusActive,
		Metadata: map[string]any{"email": "oauth@example.com"},
	}
	_, _ = manager.Register(context.Background(), oauthAuth)

	// Register API key credential (will succeed as fallback)
	apiKeyAuth := &Auth{
		ID:         "apikey-cred",
		Provider:   "test",
		Status:     StatusActive,
		Attributes: map[string]string{"api_key": "sk-test-key"},
	}
	_, _ = manager.Register(context.Background(), apiKeyAuth)

	// Execute request
	req := cliproxyexecutor.Request{Model: "test-model"}
	opts := cliproxyexecutor.Options{}
	resp, err := manager.executeWithProvider(context.Background(), "test", req, opts)

	// Verify success (via API key fallback)
	if err != nil {
		t.Fatalf("executeWithProvider() error = %v", err)
	}
	if resp.Payload == nil {
		t.Error("expected non-nil response payload")
	}

	// Verify call sequence: OAuth first, then API key
	callMu.Lock()
	defer callMu.Unlock()
	if len(callOrder) != 2 {
		t.Errorf("expected 2 calls (oauth then apikey), got %d: %v", len(callOrder), callOrder)
	}
	if len(callOrder) >= 1 && callOrder[0] != "oauth-cred" {
		t.Errorf("expected oauth-cred to be tried first, got %s", callOrder[0])
	}
	if len(callOrder) >= 2 && callOrder[1] != "apikey-cred" {
		t.Errorf("expected apikey-cred as fallback, got %s", callOrder[1])
	}
}

// TestExecuteWithProvider_AllCredentialsExhausted verifies that when both OAuth
// and API key credentials fail, the system returns the last error.
func TestExecuteWithProvider_AllCredentialsExhausted(t *testing.T) {
	t.Parallel()

	// Track which credentials were used
	var callOrder []string
	var callMu sync.Mutex

	manager := NewManager(nil, nil, nil, nil)

	// Create executor that fails for all credentials
	executor := &mockProviderExecutor{
		identifier: "test",
		executeFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			callMu.Lock()
			callOrder = append(callOrder, auth.ID)
			callMu.Unlock()

			credType := getCredentialType(auth)
			if credType == "oauth" {
				return cliproxyexecutor.Response{}, errors.New("oauth token expired")
			}
			// API key also fails
			return cliproxyexecutor.Response{}, errors.New("api key invalid")
		},
	}
	manager.RegisterExecutor(executor)

	// Register OAuth credential (will fail)
	oauthAuth := &Auth{
		ID:       "oauth-cred",
		Provider: "test",
		Status:   StatusActive,
		Metadata: map[string]any{"email": "oauth@example.com"},
	}
	_, _ = manager.Register(context.Background(), oauthAuth)

	// Register API key credential (will also fail)
	apiKeyAuth := &Auth{
		ID:         "apikey-cred",
		Provider:   "test",
		Status:     StatusActive,
		Attributes: map[string]string{"api_key": "sk-test-key"},
	}
	_, _ = manager.Register(context.Background(), apiKeyAuth)

	// Execute request
	req := cliproxyexecutor.Request{Model: "test-model"}
	opts := cliproxyexecutor.Options{}
	_, err := manager.executeWithProvider(context.Background(), "test", req, opts)

	// Verify error (all credentials exhausted returns last error)
	if err == nil {
		t.Fatal("expected error when all credentials exhausted")
	}

	// The last error should be from API key (the last tried credential)
	if !strings.Contains(err.Error(), "api key invalid") {
		t.Errorf("expected 'api key invalid' error, got: %v", err)
	}

	// Verify both credentials were tried
	callMu.Lock()
	defer callMu.Unlock()
	if len(callOrder) != 2 {
		t.Errorf("expected 2 calls (both credentials tried), got %d: %v", len(callOrder), callOrder)
	}
	if len(callOrder) >= 1 && callOrder[0] != "oauth-cred" {
		t.Errorf("expected oauth-cred to be tried first, got %s", callOrder[0])
	}
	if len(callOrder) >= 2 && callOrder[1] != "apikey-cred" {
		t.Errorf("expected apikey-cred to be tried second, got %s", callOrder[1])
	}
}

// TestExecuteWithProvider_CircuitBreakerWithFallback verifies that when OAuth
// circuit breaker is open, the system falls back to API key credential.
func TestExecuteWithProvider_CircuitBreakerWithFallback(t *testing.T) {
	t.Parallel()

	// Track which credentials were used
	var callOrder []string
	var callMu sync.Mutex

	manager := NewManager(nil, nil, nil, nil)

	// Create executor that succeeds for all credentials
	executor := &mockProviderExecutor{
		identifier: "test",
		executeFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			callMu.Lock()
			callOrder = append(callOrder, auth.ID)
			callMu.Unlock()
			return cliproxyexecutor.Response{Payload: []byte(`{"success":true}`)}, nil
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

	// Open circuit breaker for OAuth credential by recording failures
	// The default failure threshold is 5
	for i := 0; i < 5; i++ {
		if !manager.circuitBreakers.Allow("oauth-cred") {
			t.Fatalf("expected request %d to be allowed before circuit opens", i)
		}
		manager.circuitBreakers.RecordFailure("oauth-cred")
	}

	// Verify OAuth circuit is now open
	if manager.circuitBreakers.Allow("oauth-cred") {
		t.Error("expected OAuth circuit to be open after 5 failures")
	}

	// Verify API key circuit is still closed
	if !manager.circuitBreakers.Allow("apikey-cred") {
		t.Error("expected API key circuit to be closed")
	}

	// Execute request - should skip OAuth (circuit open) and use API key directly
	req := cliproxyexecutor.Request{Model: "test-model"}
	opts := cliproxyexecutor.Options{}
	resp, err := manager.executeWithProvider(context.Background(), "test", req, opts)

	// Verify success (via API key since OAuth circuit is open)
	if err != nil {
		t.Fatalf("executeWithProvider() error = %v", err)
	}
	if resp.Payload == nil {
		t.Error("expected non-nil response payload")
	}

	// Verify only API key credential was used (OAuth skipped due to open circuit)
	callMu.Lock()
	defer callMu.Unlock()
	if len(callOrder) != 1 {
		t.Errorf("expected 1 call (apikey only), got %d: %v", len(callOrder), callOrder)
	}
	if len(callOrder) > 0 && callOrder[0] != "apikey-cred" {
		t.Errorf("expected apikey-cred to be used (OAuth circuit open), got %s", callOrder[0])
	}
}
