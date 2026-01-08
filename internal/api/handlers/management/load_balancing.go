package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// HealthStatus represents health information for a single credential.
type HealthStatus struct {
	AuthID              string  `json:"auth_id"`
	Provider            string  `json:"provider"`
	DisplayName         string  `json:"display_name,omitempty"`
	Healthy             bool    `json:"healthy"`
	Verified            bool    `json:"verified"`
	AuthType            string  `json:"auth_type"` // "api_key" or "oauth"
	LastCheck           *string `json:"last_check"`
	LastHealthCheck     *string `json:"last_health_check,omitempty"`
	LastError           *string `json:"last_error"`
	ConsecutiveFailures int     `json:"consecutive_failures"`
	HealthCheckCount    int     `json:"health_check_count"`
	CircuitState        string  `json:"circuit_state,omitempty"`
}

// CircuitBreakerStatus represents circuit breaker state for a credential.
type CircuitBreakerStatus struct {
	AuthID       string  `json:"auth_id"`
	Provider     string  `json:"provider"`
	DisplayName  string  `json:"display_name,omitempty"`
	State        string  `json:"state"`
	FailureCount int     `json:"failure_count"`
	SuccessCount int     `json:"success_count"`
	LastFailure  *string `json:"last_failure"`
	NextRetry    *string `json:"next_retry"`
}

// GetHealthStatus returns health status for all credentials.
// GET /v0/management/health-status
func (h *Handler) GetHealthStatus(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager not available"})
		return
	}

	auths := h.authManager.List()
	cbStats := h.authManager.CircuitBreakerStats()
	statuses := make([]HealthStatus, 0, len(auths))

	for _, auth := range auths {
		if auth == nil {
			continue
		}

		displayName := strings.TrimSpace(auth.Label)
		if displayName == "" {
			displayName = strings.TrimSpace(auth.FileName)
		}
		if displayName == "" {
			displayName = auth.ID
		}

		// Determine auth type and health status
		isAPIKey := auth.Attributes != nil && auth.Attributes["api_key"] != ""
		authType := "oauth"
		if isAPIKey {
			authType = "api_key"
		}

		// Determine if credential is healthy:
		// - API key credentials are healthy by default (can't be verified via Refresh)
		// - OAuth credentials need to be verified (via successful execution or health check)
		healthy := !auth.Unavailable && (isAPIKey || auth.Verified)

		status := HealthStatus{
			AuthID:              auth.ID,
			Provider:            auth.Provider,
			DisplayName:         displayName,
			Healthy:             healthy,
			Verified:            auth.Verified,
			AuthType:            authType,
			ConsecutiveFailures: auth.HealthCheckFails,
			HealthCheckCount:    auth.HealthCheckCount,
		}

		if !auth.UpdatedAt.IsZero() {
			t := auth.UpdatedAt.Format(time.RFC3339)
			status.LastCheck = &t
		}

		if !auth.LastHealthCheck.IsZero() {
			t := auth.LastHealthCheck.Format(time.RFC3339)
			status.LastHealthCheck = &t
		}

		if auth.LastHealthError != "" {
			status.LastError = &auth.LastHealthError
		} else if auth.StatusMessage != "" {
			status.LastError = &auth.StatusMessage
		}

		// Include circuit breaker state
		if stats, ok := cbStats[auth.ID]; ok {
			status.CircuitState = stats.State.String()
		} else {
			status.CircuitState = "closed"
		}

		statuses = append(statuses, status)
	}

	c.JSON(http.StatusOK, statuses)
}

// GetCircuitBreakerStatus returns circuit breaker status for all credentials.
// GET /v0/management/circuit-breakers
func (h *Handler) GetCircuitBreakerStatus(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager not available"})
		return
	}

	auths := h.authManager.List()
	cbStats := h.authManager.CircuitBreakerStats()

	statuses := make([]CircuitBreakerStatus, 0, len(auths))

	for _, auth := range auths {
		if auth == nil {
			continue
		}

		displayName := strings.TrimSpace(auth.Label)
		if displayName == "" {
			displayName = strings.TrimSpace(auth.FileName)
		}
		if displayName == "" {
			displayName = auth.ID
		}

		status := CircuitBreakerStatus{
			AuthID:       auth.ID,
			Provider:     auth.Provider,
			DisplayName:  displayName,
			State:        "closed",
			FailureCount: 0,
			SuccessCount: 0,
		}

		if stats, ok := cbStats[auth.ID]; ok {
			status.State = stats.State.String()
			status.FailureCount = stats.ConsecutiveFailures
			status.SuccessCount = stats.ConsecutiveSuccesses
			if !stats.LastFailureTime.IsZero() && stats.LastFailureTime.Unix() > 0 {
				t := stats.LastFailureTime.Format(time.RFC3339)
				status.LastFailure = &t
			}
		}

		statuses = append(statuses, status)
	}

	c.JSON(http.StatusOK, statuses)
}

// ResetCircuitBreaker resets the circuit breaker for a specific credential.
// POST /v0/management/circuit-breakers/:auth_id/reset
func (h *Handler) ResetCircuitBreaker(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager not available"})
		return
	}

	authID := c.Param("auth_id")
	if authID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_id is required"})
		return
	}

	h.authManager.ResetCircuitBreaker(authID)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "auth_id": authID})
}

// TriggerHealthCheck manually triggers a health check for a specific credential.
// POST /v0/management/health-check/trigger/:auth_id
func (h *Handler) TriggerHealthCheck(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager not available"})
		return
	}

	authID := c.Param("auth_id")
	if authID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_id is required"})
		return
	}

	// Get the auth and trigger a refresh
	auth, exists := h.authManager.GetByID(authID)
	if !exists || auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential not found"})
		return
	}

	// Trigger refresh which acts as a health check
	err := h.authManager.TriggerRefresh(c.Request.Context(), authID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status":  "error",
			"auth_id": authID,
			"error":   err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"auth_id": authID,
		"message": "health check triggered",
	})
}

// ResetHealthStatus resets the health status for a specific credential.
// POST /v0/management/health-status/:auth_id/reset
func (h *Handler) ResetHealthStatus(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager not available"})
		return
	}

	authID := c.Param("auth_id")
	if authID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_id is required"})
		return
	}

	// Reset health status by clearing unavailable flag and health check failures
	err := h.authManager.ResetHealthStatus(authID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential not found"})
		return
	}

	// Also reset circuit breaker
	h.authManager.ResetCircuitBreaker(authID)

	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"auth_id": authID,
		"message": "health status reset",
	})
}
