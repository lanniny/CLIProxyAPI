package management

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// CredentialWeight represents weight information for a single credential.
type CredentialWeight struct {
	AuthID      string `json:"auth_id"`
	Provider    string `json:"provider"`
	Weight      int    `json:"weight"`
	DisplayName string `json:"display_name,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
}

// GetCredentialWeights returns weights for all credentials.
// GET /v0/management/credential-weights
func (h *Handler) GetCredentialWeights(c *gin.Context) {
	clientIP := c.ClientIP()
	log.Infof("GetCredentialWeights called from client %s", clientIP)

	if h.authManager == nil {
		log.Warnf("GetCredentialWeights: auth manager not available for client %s", clientIP)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager not available"})
		return
	}

	log.Debugf("GetCredentialWeights: authManager is available for client %s", clientIP)

	auths := h.authManager.List()
	log.Debugf("GetCredentialWeights: found %d auths from authManager.List() for client %s", len(auths), clientIP)

	weights := make([]CredentialWeight, 0, len(auths))

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

		weights = append(weights, CredentialWeight{
			AuthID:      auth.ID,
			Provider:    auth.Provider,
			Weight:      auth.Weight,
			DisplayName: displayName,
			Disabled:    auth.Disabled,
		})
	}

	log.Debugf("GetCredentialWeights: returning %d weights for client %s", len(weights), clientIP)
	if len(weights) > 0 {
		log.Debugf("GetCredentialWeights: sample weight data - auth_id=%s, provider=%s, weight=%d",
			weights[0].AuthID, weights[0].Provider, weights[0].Weight)
	}

	c.JSON(http.StatusOK, gin.H{"weights": weights})
}

// PutCredentialWeightsRequest represents the request body for updating credential weights.
type PutCredentialWeightsRequest struct {
	Weights []CredentialWeightUpdate `json:"weights"`
}

// CredentialWeightUpdate represents a weight update for a single credential.
type CredentialWeightUpdate struct {
	AuthID string `json:"auth_id"`
	Weight int    `json:"weight"`
}

// PutCredentialWeights updates weights for multiple credentials.
// PUT /v0/management/credential-weights
func (h *Handler) PutCredentialWeights(c *gin.Context) {
	clientIP := c.ClientIP()
	log.Infof("PutCredentialWeights called from client %s", clientIP)

	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager not available"})
		return
	}

	var req PutCredentialWeightsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if len(req.Weights) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "weights array is required"})
		return
	}

	ctx := c.Request.Context()
	updated := make([]string, 0, len(req.Weights))
	notFound := make([]string, 0)
	errors := make([]gin.H, 0)

	for _, update := range req.Weights {
		authID := strings.TrimSpace(update.AuthID)
		if authID == "" {
			errors = append(errors, gin.H{
				"auth_id": "",
				"error":   "auth_id cannot be empty",
			})
			continue
		}

		if update.Weight < 0 {
			errors = append(errors, gin.H{
				"auth_id": authID,
				"error":   "weight must be >= 0",
			})
			continue
		}

		if err := h.updateCredentialWeight(ctx, authID, update.Weight); err != nil {
			if _, ok := err.(*notFoundError); ok {
				notFound = append(notFound, authID)
			} else {
				errors = append(errors, gin.H{
					"auth_id": authID,
					"error":   err.Error(),
				})
			}
			continue
		}
		updated = append(updated, authID)
	}

	response := gin.H{
		"status":  "ok",
		"updated": updated,
	}
	if len(notFound) > 0 {
		response["not_found"] = notFound
	}
	if len(errors) > 0 {
		response["errors"] = errors
	}

	log.Infof("PutCredentialWeights: updated=%d, not_found=%d, errors=%d for client %s",
		len(updated), len(notFound), len(errors), clientIP)

	c.JSON(http.StatusOK, response)
}

// updateCredentialWeight updates the weight for a single credential.
func (h *Handler) updateCredentialWeight(ctx context.Context, authID string, weight int) error {
	auth, ok := h.authManager.GetByID(authID)
	if !ok {
		auths := h.authManager.List()
		for _, a := range auths {
			if a.FileName == authID || a.ID == authID {
				auth = a
				ok = true
				break
			}
		}
	}

	if !ok || auth == nil {
		return &notFoundError{authID: authID}
	}

	auth.Weight = weight
	_, err := h.authManager.Update(ctx, auth)
	return err
}

type notFoundError struct {
	authID string
}

func (e *notFoundError) Error() string {
	return "credential not found: " + e.authID
}
