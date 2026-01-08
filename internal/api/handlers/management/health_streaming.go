package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// StreamHealthStatus streams health status updates via SSE.
// GET /v0/management/health-status/stream
func (h *Handler) StreamHealthStatus(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager not available"})
		return
	}

	// Set SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Access-Control-Allow-Origin", "*")

	// Get flusher
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming not supported"})
		return
	}

	// Create ticker for periodic updates
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	ctx := c.Request.Context()

	// Send initial data immediately
	h.sendHealthUpdate(c, flusher)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.sendHealthUpdate(c, flusher)
		}
	}
}

func (h *Handler) sendHealthUpdate(c *gin.Context, flusher http.Flusher) {
	// Get health status
	healthStatuses := h.getHealthStatuses()

	// Get circuit breaker status
	cbStatuses := h.getCircuitBreakerStatuses()

	// Send health-update event
	if data, err := json.Marshal(healthStatuses); err == nil {
		fmt.Fprintf(c.Writer, "event: health-update\ndata: %s\n\n", data)
	}

	// Send circuit-breaker-update event
	if data, err := json.Marshal(cbStatuses); err == nil {
		fmt.Fprintf(c.Writer, "event: circuit-breaker-update\ndata: %s\n\n", data)
	}

	flusher.Flush()
}

func (h *Handler) getHealthStatuses() []HealthStatus {
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

		isAPIKey := auth.Attributes != nil && auth.Attributes["api_key"] != ""
		authType := "oauth"
		if isAPIKey {
			authType = "api_key"
		}

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

		if stats, ok := cbStats[auth.ID]; ok {
			status.CircuitState = stats.State.String()
		} else {
			status.CircuitState = "closed"
		}

		statuses = append(statuses, status)
	}

	return statuses
}

func (h *Handler) getCircuitBreakerStatuses() []CircuitBreakerStatus {
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

	return statuses
}
