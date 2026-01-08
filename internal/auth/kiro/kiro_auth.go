// Package kiro provides OAuth2 authentication functionality for AWS Kiro API.
// This package implements OAuth2 flows for both social login (Google/GitHub)
// and AWS Identity Center (IdC) authentication.
package kiro

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	// Default region for Kiro services
	DefaultRegion = "us-east-1"

	// Social auth token refresh endpoint template
	SocialRefreshURLTemplate = "https://prod.%s.auth.desktop.kiro.dev/refreshToken"

	// IdC token endpoint template
	IdCTokenURLTemplate = "https://oidc.%s.amazonaws.com/token"

	// CodeWhisperer API endpoint template
	CodeWhispererAPITemplate = "https://codewhisperer.%s.amazonaws.com"
)

// socialRefreshResponse represents the response from social auth token refresh.
type socialRefreshResponse struct {
	AccessToken string `json:"accessToken"`
	ExpiresIn   int64  `json:"expiresIn"`
}

// idcTokenResponse represents the response from IdC token endpoint.
type idcTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

// KiroAuth handles Kiro OAuth2 authentication flow.
// It provides methods for refreshing tokens for both social and IdC authentication.
type KiroAuth struct {
	httpClient *http.Client
}

// NewKiroAuth creates a new Kiro authentication service.
//
// Parameters:
//   - cfg: The application configuration containing proxy settings
//
// Returns:
//   - *KiroAuth: A new Kiro authentication service instance
func NewKiroAuth(cfg *config.Config) *KiroAuth {
	return &KiroAuth{
		httpClient: util.SetProxy(&cfg.SDKConfig, &http.Client{
			Timeout: 30 * time.Second,
		}),
	}
}

// RefreshSocialToken refreshes an access token using social auth (Google/GitHub).
//
// Parameters:
//   - ctx: The context for the request
//   - refreshToken: The refresh token to use
//   - region: The AWS region (defaults to us-east-1)
//
// Returns:
//   - *KiroTokenData: The new token data
//   - error: An error if refresh fails
func (k *KiroAuth) RefreshSocialToken(ctx context.Context, refreshToken, region string) (*KiroTokenData, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh token is required")
	}

	if region == "" {
		region = DefaultRegion
	}

	refreshURL := fmt.Sprintf(SocialRefreshURLTemplate, region)

	reqBody := map[string]string{
		"refreshToken": refreshToken,
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshURL, strings.NewReader(string(jsonBody)))
	if err != nil {
		return nil, fmt.Errorf("failed to create refresh request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", generateKiroUserAgent())

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token refresh request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token refresh failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp socialRefreshResponse
	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}

	expiresIn := tokenResp.ExpiresIn
	if expiresIn == 0 {
		expiresIn = 3600 // Default 1 hour
	}

	return &KiroTokenData{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339),
	}, nil
}

// RefreshIdCToken refreshes an access token using AWS Identity Center.
//
// Parameters:
//   - ctx: The context for the request
//   - refreshToken: The refresh token to use
//   - clientID: The OAuth client ID
//   - clientSecret: The OAuth client secret
//   - region: The AWS region (defaults to us-east-1)
//
// Returns:
//   - *KiroTokenData: The new token data
//   - error: An error if refresh fails
func (k *KiroAuth) RefreshIdCToken(ctx context.Context, refreshToken, clientID, clientSecret, region string) (*KiroTokenData, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh token is required")
	}
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("client ID and client secret are required for IdC auth")
	}

	if region == "" {
		region = DefaultRegion
	}

	tokenURL := fmt.Sprintf(IdCTokenURLTemplate, region)

	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("client_id", clientID)
	data.Set("client_secret", clientSecret)
	data.Set("refresh_token", refreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create token request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "aws-sdk-js/3.738.0")

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp idcTokenResponse
	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}

	newRefreshToken := tokenResp.RefreshToken
	if newRefreshToken == "" {
		newRefreshToken = refreshToken
	}

	expiresIn := tokenResp.ExpiresIn
	if expiresIn == 0 {
		expiresIn = 3600 // Default 1 hour
	}

	return &KiroTokenData{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: newRefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339),
	}, nil
}

// RefreshTokens refreshes access token based on auth method.
//
// Parameters:
//   - ctx: The context for the request
//   - storage: The current token storage
//
// Returns:
//   - *KiroTokenData: The new token data
//   - error: An error if refresh fails
func (k *KiroAuth) RefreshTokens(ctx context.Context, storage *KiroTokenStorage) (*KiroTokenData, error) {
	if storage == nil {
		return nil, fmt.Errorf("token storage is required")
	}

	region := storage.Region
	if region == "" {
		region = DefaultRegion
	}

	switch strings.ToLower(storage.AuthMethod) {
	case "idc", "builderid":
		return k.RefreshIdCToken(ctx, storage.RefreshToken, storage.ClientID, storage.ClientSecret, region)
	case "social", "google", "github", "":
		return k.RefreshSocialToken(ctx, storage.RefreshToken, region)
	default:
		log.Warnf("Unknown auth method %s, trying social refresh", storage.AuthMethod)
		return k.RefreshSocialToken(ctx, storage.RefreshToken, region)
	}
}

// CreateTokenStorage creates a new KiroTokenStorage from auth bundle.
//
// Parameters:
//   - bundle: The authentication bundle containing token data
//
// Returns:
//   - *KiroTokenStorage: A new token storage instance
func (k *KiroAuth) CreateTokenStorage(bundle *KiroAuthBundle) *KiroTokenStorage {
	return &KiroTokenStorage{
		AccessToken:  bundle.TokenData.AccessToken,
		RefreshToken: bundle.TokenData.RefreshToken,
		ClientID:     bundle.ClientID,
		ClientSecret: bundle.ClientSecret,
		ProfileArn:   bundle.ProfileArn,
		Region:       bundle.Region,
		AuthMethod:   bundle.AuthMethod,
		Email:        bundle.TokenData.Email,
		ExpiresAt:    bundle.TokenData.ExpiresAt,
		Type:         "kiro",
	}
}

// UpdateTokenStorage updates an existing token storage with new token data.
//
// Parameters:
//   - storage: The existing token storage to update
//   - tokenData: The new token data to apply
func (k *KiroAuth) UpdateTokenStorage(storage *KiroTokenStorage, tokenData *KiroTokenData) {
	storage.AccessToken = tokenData.AccessToken
	if tokenData.RefreshToken != "" {
		storage.RefreshToken = tokenData.RefreshToken
	}
	storage.ExpiresAt = tokenData.ExpiresAt
	if tokenData.Email != "" {
		storage.Email = tokenData.Email
	}
}

// generateKiroUserAgent generates a User-Agent string for Kiro requests.
func generateKiroUserAgent() string {
	return "KiroIDE-1.0.0-cli-proxy-api"
}
