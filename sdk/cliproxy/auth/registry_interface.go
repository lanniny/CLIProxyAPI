// Package auth provides core authentication and execution management for CLI Proxy API.
package auth

// ModelRegistry defines the interface for model availability tracking used by Manager.
// This interface abstracts the model registry operations to decouple the auth package
// from internal/registry, enabling cleaner SDK-internal boundaries.
type ModelRegistry interface {
	// ClientSupportsModel checks if a specific client supports a model.
	// Returns true if the client has the model registered and available.
	ClientSupportsModel(clientID, modelID string) bool

	// SetModelQuotaExceeded marks a model as quota-exceeded for a specific client.
	// This is called when a 429 response indicates the client has hit rate limits.
	SetModelQuotaExceeded(clientID, modelID string)

	// ClearModelQuotaExceeded removes the quota-exceeded status for a client-model pair.
	// This is called when a successful request indicates quota has recovered.
	ClearModelQuotaExceeded(clientID, modelID string)

	// SuspendClientModel temporarily disables a client-model pair with a reason.
	// Used for non-quota failures like 401, 403, 404 errors.
	SuspendClientModel(clientID, modelID, reason string)

	// ResumeClientModel re-enables a previously suspended client-model pair.
	// Called when a successful request indicates the client can serve the model again.
	ResumeClientModel(clientID, modelID string)
}

// noopModelRegistry is a no-op implementation used when no registry is provided.
type noopModelRegistry struct{}

func (noopModelRegistry) ClientSupportsModel(_, _ string) bool { return true }
func (noopModelRegistry) SetModelQuotaExceeded(_, _ string)    {}
func (noopModelRegistry) ClearModelQuotaExceeded(_, _ string)  {}
func (noopModelRegistry) SuspendClientModel(_, _, _ string)    {}
func (noopModelRegistry) ResumeClientModel(_, _ string)        {}
