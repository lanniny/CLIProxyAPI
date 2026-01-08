package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	kiroCallbackPort         = 23001
	kiroDefaultRegion        = "us-east-1"
	kiroSocialRefreshURL     = "https://prod.%s.auth.desktop.kiro.dev/refreshToken"
	kiroIdCTokenURL          = "https://oidc.%s.amazonaws.com/token"
	kiroAuthType             = "kiro"
)

// KiroAuthenticator implements OAuth login for the Kiro provider.
type KiroAuthenticator struct{}

// NewKiroAuthenticator constructs a new authenticator instance.
func NewKiroAuthenticator() Authenticator { return &KiroAuthenticator{} }

// Provider returns the provider key for kiro.
func (KiroAuthenticator) Provider() string { return kiroAuthType }

// RefreshLead instructs the manager to refresh five minutes before expiry.
func (KiroAuthenticator) RefreshLead() *time.Duration {
	lead := 5 * time.Minute
	return &lead
}

// Login handles Kiro credential import (manual paste of token JSON).
// Since Kiro uses browser-based OAuth through the Kiro IDE, we support
// importing existing credentials from the kiro-auth-token.json file.
func (KiroAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	fmt.Println("Kiro Authentication")
	fmt.Println("==================")
	fmt.Println("")
	fmt.Println("Kiro uses browser-based OAuth through the Kiro IDE.")
	fmt.Println("To import your credentials:")
	fmt.Println("")
	fmt.Println("1. Open Kiro IDE and sign in with Google, GitHub, or AWS Builder ID")
	fmt.Println("2. Locate your credentials file:")
	fmt.Println("   - Windows: " + `%USERPROFILE%\.kiro\kiro-auth-token.json`)
	fmt.Println("   - macOS/Linux: ~/.kiro/kiro-auth-token.json")
	fmt.Println("3. Copy the contents of this file")
	fmt.Println("")

	if opts.Prompt == nil {
		return nil, fmt.Errorf("prompt function is required for Kiro login")
	}

	input, err := opts.Prompt("Paste your Kiro credentials JSON (or press Enter to cancel): ")
	if err != nil {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}

	input = strings.TrimSpace(input)
	if input == "" {
		return nil, fmt.Errorf("kiro: login cancelled")
	}

	// Parse the credentials JSON
	var creds struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		ProfileArn   string `json:"profileArn"`
		Region       string `json:"region"`
		AuthMethod   string `json:"authMethod"`
		ExpiresAt    string `json:"expiresAt"`
	}

	if err := json.Unmarshal([]byte(input), &creds); err != nil {
		return nil, fmt.Errorf("kiro: failed to parse credentials JSON: %w", err)
	}

	if creds.RefreshToken == "" {
		return nil, fmt.Errorf("kiro: refreshToken is required in credentials")
	}

	region := creds.Region
	if region == "" {
		region = kiroDefaultRegion
	}

	authMethod := creds.AuthMethod
	if authMethod == "" {
		authMethod = "social"
	}

	// Try to refresh the token to verify credentials work
	httpClient := util.SetProxy(&cfg.SDKConfig, &http.Client{Timeout: 30 * time.Second})

	var accessToken string
	var expiresAt string

	if strings.ToLower(authMethod) == "idc" || strings.ToLower(authMethod) == "builderid" {
		if creds.ClientID == "" || creds.ClientSecret == "" {
			return nil, fmt.Errorf("kiro: clientId and clientSecret are required for IdC auth")
		}
		tokenData, err := refreshKiroIdCToken(ctx, creds.RefreshToken, creds.ClientID, creds.ClientSecret, region, httpClient)
		if err != nil {
			log.Warnf("kiro: token refresh failed, using provided token: %v", err)
			accessToken = creds.AccessToken
			expiresAt = creds.ExpiresAt
		} else {
			accessToken = tokenData.AccessToken
			expiresAt = tokenData.ExpiresAt
			if tokenData.RefreshToken != "" {
				creds.RefreshToken = tokenData.RefreshToken
			}
		}
	} else {
		tokenData, err := refreshKiroSocialToken(ctx, creds.RefreshToken, region, httpClient)
		if err != nil {
			log.Warnf("kiro: token refresh failed, using provided token: %v", err)
			accessToken = creds.AccessToken
			expiresAt = creds.ExpiresAt
		} else {
			accessToken = tokenData.AccessToken
			expiresAt = tokenData.ExpiresAt
		}
	}

	if accessToken == "" {
		accessToken = creds.AccessToken
	}
	if expiresAt == "" {
		expiresAt = time.Now().Add(1 * time.Hour).Format(time.RFC3339)
	}

	// Extract email from profile ARN or use a default identifier
	email := ""
	if creds.ProfileArn != "" {
		parts := strings.Split(creds.ProfileArn, ":")
		if len(parts) > 4 {
			email = parts[4] // Account ID
		}
	}

	now := time.Now()
	metadata := map[string]any{
		"type":          kiroAuthType,
		"access_token":  accessToken,
		"refresh_token": creds.RefreshToken,
		"auth_method":   authMethod,
		"region":        region,
		"timestamp":     now.UnixMilli(),
		"expired":       expiresAt,
	}

	if creds.ClientID != "" {
		metadata["client_id"] = creds.ClientID
	}
	if creds.ClientSecret != "" {
		metadata["client_secret"] = creds.ClientSecret
	}
	if creds.ProfileArn != "" {
		metadata["profile_arn"] = creds.ProfileArn
	}
	if email != "" {
		metadata["email"] = email
	}

	fileName := sanitizeKiroFileName(email, authMethod)
	label := "kiro"
	if email != "" {
		label = email
	}

	fmt.Println("Kiro authentication successful!")
	fmt.Printf("Auth method: %s, Region: %s\n", authMethod, region)

	return &coreauth.Auth{
		ID:       fileName,
		Provider: kiroAuthType,
		FileName: fileName,
		Label:    label,
		Metadata: metadata,
	}, nil
}

type kiroTokenResponse struct {
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int64  `json:"expiresIn,omitempty"`
	ExpiresAt    string
}

func refreshKiroSocialToken(ctx context.Context, refreshToken, region string, httpClient *http.Client) (*kiroTokenResponse, error) {
	if region == "" {
		region = kiroDefaultRegion
	}

	refreshURL := fmt.Sprintf(kiroSocialRefreshURL, region)

	reqBody := map[string]string{
		"refreshToken": refreshToken,
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshURL, strings.NewReader(string(jsonBody)))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "KiroIDE-1.0.0-cli-proxy-api")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token refresh failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"accessToken"`
		ExpiresIn   int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, err
	}

	expiresIn := tokenResp.ExpiresIn
	if expiresIn == 0 {
		expiresIn = 3600
	}

	return &kiroTokenResponse{
		AccessToken: tokenResp.AccessToken,
		ExpiresAt:   time.Now().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339),
	}, nil
}

func refreshKiroIdCToken(ctx context.Context, refreshToken, clientID, clientSecret, region string, httpClient *http.Client) (*kiroTokenResponse, error) {
	if region == "" {
		region = kiroDefaultRegion
	}

	tokenURL := fmt.Sprintf(kiroIdCTokenURL, region)

	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("client_id", clientID)
	data.Set("client_secret", clientSecret)
	data.Set("refresh_token", refreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "aws-sdk-js/3.738.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, err
	}

	expiresIn := tokenResp.ExpiresIn
	if expiresIn == 0 {
		expiresIn = 3600
	}

	return &kiroTokenResponse{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339),
	}, nil
}

func sanitizeKiroFileName(email, authMethod string) string {
	base := "kiro"
	if authMethod != "" && authMethod != "social" {
		base = fmt.Sprintf("kiro-%s", strings.ToLower(authMethod))
	}
	if strings.TrimSpace(email) == "" {
		return base + ".json"
	}
	replacer := strings.NewReplacer("@", "_", ".", "_")
	return fmt.Sprintf("%s-%s.json", base, replacer.Replace(email))
}

// Unused but kept for potential future browser-based OAuth flow
var _ = startKiroCallbackServer

func startKiroCallbackServer() (*http.Server, int, <-chan callbackResult, error) {
	addr := fmt.Sprintf(":%d", kiroCallbackPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, 0, nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	resultCh := make(chan callbackResult, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		res := callbackResult{
			Code:  strings.TrimSpace(q.Get("code")),
			Error: strings.TrimSpace(q.Get("error")),
			State: strings.TrimSpace(q.Get("state")),
		}
		resultCh <- res
		if res.Code != "" && res.Error == "" {
			_, _ = w.Write([]byte("<h1>Login successful</h1><p>You can close this window.</p>"))
		} else {
			_, _ = w.Write([]byte("<h1>Login failed</h1><p>Please check the CLI output.</p>"))
		}
	})

	srv := &http.Server{Handler: mux}
	go func() {
		if errServe := srv.Serve(listener); errServe != nil && !strings.Contains(errServe.Error(), "Server closed") {
			log.Warnf("kiro callback server error: %v", errServe)
		}
	}()

	return srv, port, resultCh, nil
}

// Suppress unused import warnings
var (
	_ = browser.IsAvailable
	_ = misc.GenerateRandomState
)
