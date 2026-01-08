// Package kiro provides authentication and token management functionality
// for AWS Kiro AI services. It handles OAuth2 token storage, serialization,
// and retrieval for maintaining authenticated sessions with the Kiro API.
package kiro

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
)

// KiroTokenStorage stores OAuth2 token information for Kiro API authentication.
// It maintains compatibility with the existing auth system while adding Kiro-specific fields
// for managing access tokens, refresh tokens, and user account information.
type KiroTokenStorage struct {
	// AccessToken is the OAuth2 access token used for authenticating API requests.
	AccessToken string `json:"accessToken"`

	// RefreshToken is used to obtain new access tokens when the current one expires.
	RefreshToken string `json:"refreshToken"`

	// ClientID is the OAuth client identifier (required for IdC auth).
	ClientID string `json:"clientId,omitempty"`

	// ClientSecret is the OAuth client secret (required for IdC auth).
	ClientSecret string `json:"clientSecret,omitempty"`

	// ProfileArn is the AWS IAM profile ARN.
	ProfileArn string `json:"profileArn,omitempty"`

	// Region is the AWS region (default: us-east-1).
	Region string `json:"region,omitempty"`

	// AuthMethod is the authentication method: "social" or "idc".
	AuthMethod string `json:"authMethod"`

	// Email is the account email address associated with this token.
	Email string `json:"email,omitempty"`

	// Type indicates the authentication provider type, always "kiro" for this storage.
	Type string `json:"type"`

	// ExpiresAt is the timestamp when the current access token expires (RFC3339).
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// SaveTokenToFile serializes the Kiro token storage to a JSON file.
// This method creates the necessary directory structure and writes the token
// data in JSON format to the specified file path for persistent storage.
//
// Parameters:
//   - authFilePath: The full path where the token file should be saved
//
// Returns:
//   - error: An error if the operation fails, nil otherwise
func (ts *KiroTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = "kiro"

	if ts.Region == "" {
		ts.Region = "us-east-1"
	}

	// Create directory structure if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(authFilePath), 0700); err != nil {
		return fmt.Errorf("failed to create directory: %v", err)
	}

	// Create the token file
	f, err := os.Create(authFilePath)
	if err != nil {
		return fmt.Errorf("failed to create token file: %w", err)
	}
	defer func() {
		_ = f.Close()
	}()

	// Encode and write the token data as JSON
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(ts); err != nil {
		return fmt.Errorf("failed to write token to file: %w", err)
	}
	return nil
}

// KiroTokenData represents the token data received from OAuth token exchange.
type KiroTokenData struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    string `json:"expiresAt"`
	Email        string `json:"email,omitempty"`
}

// KiroAuthBundle contains the complete authentication bundle for Kiro.
type KiroAuthBundle struct {
	TokenData   KiroTokenData
	AuthMethod  string
	Region      string
	ClientID    string
	ClientSecret string
	ProfileArn  string
	LastRefresh string
}
