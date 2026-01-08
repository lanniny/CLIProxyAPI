// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements the Kiro executor that proxies requests to the AWS CodeWhisperer
// upstream using OAuth credentials.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	kiroDefaultRegion         = "us-east-1"
	kiroQAPIBase              = "https://q.%s.amazonaws.com"
	kiroGenerateAssistantPath = "/generateAssistantResponse"
	kiroSocialRefreshURL      = "https://prod.%s.auth.desktop.kiro.dev/refreshToken"
	kiroIdCTokenURL           = "https://oidc.%s.amazonaws.com/token"
	kiroAuthType              = "kiro"
	kiroVersion               = "0.8.0"
	kiroNodeVersion           = "22.21.1"
	kiroRefreshSkew           = 300 * time.Second // 5 minutes before expiry
)

// KiroExecutor proxies requests to the Kiro/CodeWhisperer upstream.
type KiroExecutor struct {
	cfg *config.Config
}

// NewKiroExecutor creates a new Kiro executor instance.
//
// Parameters:
//   - cfg: The application configuration
//
// Returns:
//   - *KiroExecutor: A new Kiro executor instance
func NewKiroExecutor(cfg *config.Config) *KiroExecutor {
	return &KiroExecutor{cfg: cfg}
}

// Identifier returns the executor identifier.
func (e *KiroExecutor) Identifier() string { return kiroAuthType }

// PrepareRequest prepares the HTTP request for execution (no-op for Kiro).
func (e *KiroExecutor) PrepareRequest(_ *http.Request, _ *cliproxyauth.Auth) error { return nil }

// Execute performs a non-streaming request to the Kiro API.
func (e *KiroExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return resp, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	reporter := newUsageReporter(ctx, e.Identifier(), req.Model, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("kiro")
	originalPayload := bytes.Clone(req.Payload)
	if len(opts.OriginalRequest) > 0 {
		originalPayload = bytes.Clone(opts.OriginalRequest)
	}

	translated := sdktranslator.TranslateRequest(from, to, req.Model, bytes.Clone(req.Payload), false)
	translated = e.applyKiroPayload(translated, req.Model, auth)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	region := e.getRegion(auth)
	baseURL := fmt.Sprintf(kiroQAPIBase, region)

	httpReq, errReq := e.buildRequest(ctx, auth, token, translated, baseURL)
	if errReq != nil {
		err = errReq
		return resp, err
	}

	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		recordAPIResponseError(ctx, e.cfg, errDo)
		err = errDo
		return resp, err
	}

	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	bodyBytes, errRead := io.ReadAll(httpResp.Body)
	if errClose := httpResp.Body.Close(); errClose != nil {
		log.Errorf("kiro executor: close response body error: %v", errClose)
	}
	if errRead != nil {
		recordAPIResponseError(ctx, e.cfg, errRead)
		err = errRead
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, bodyBytes)

	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		log.Debugf("kiro executor: upstream error status: %d, body: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), bodyBytes))
		err = statusErr{code: httpResp.StatusCode, msg: string(bodyBytes)}
		return resp, err
	}

	reporter.publish(ctx, parseKiroUsage(bodyBytes))
	var param any
	converted := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, bytes.Clone(originalPayload), translated, bodyBytes, &param)
	resp = cliproxyexecutor.Response{Payload: []byte(converted)}
	reporter.ensurePublished(ctx)
	return resp, nil
}

// ExecuteStream performs a streaming request to the Kiro API.
func (e *KiroExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (stream <-chan cliproxyexecutor.StreamChunk, err error) {
	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return nil, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	reporter := newUsageReporter(ctx, e.Identifier(), req.Model, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("kiro")
	originalPayload := bytes.Clone(req.Payload)
	if len(opts.OriginalRequest) > 0 {
		originalPayload = bytes.Clone(opts.OriginalRequest)
	}

	translated := sdktranslator.TranslateRequest(from, to, req.Model, bytes.Clone(req.Payload), true)
	translated = e.applyKiroPayload(translated, req.Model, auth)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	region := e.getRegion(auth)
	baseURL := fmt.Sprintf(kiroQAPIBase, region)

	httpReq, errReq := e.buildRequest(ctx, auth, token, translated, baseURL)
	if errReq != nil {
		err = errReq
		return nil, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		recordAPIResponseError(ctx, e.cfg, errDo)
		err = errDo
		return nil, err
	}

	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kiro executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			recordAPIResponseError(ctx, e.cfg, errRead)
			err = errRead
			return nil, err
		}
		appendAPIResponseChunk(ctx, e.cfg, bodyBytes)
		err = statusErr{code: httpResp.StatusCode, msg: string(bodyBytes)}
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	stream = out
	go func(resp *http.Response) {
		defer close(out)
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Errorf("kiro executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(nil, streamScannerBuffer)
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)

			payload := jsonPayload(line)
			if payload == nil {
				continue
			}

			if detail, ok := parseKiroStreamUsage(payload); ok {
				reporter.publish(ctx, detail)
			}

			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, bytes.Clone(originalPayload), translated, bytes.Clone(payload), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
		tail := sdktranslator.TranslateStream(ctx, to, from, req.Model, bytes.Clone(originalPayload), translated, []byte("[DONE]"), &param)
		for i := range tail {
			out <- cliproxyexecutor.StreamChunk{Payload: []byte(tail[i])}
		}
		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		} else {
			reporter.ensurePublished(ctx)
		}
	}(httpResp)
	return stream, nil
}

// Refresh refreshes the authentication credentials using the refresh token.
func (e *KiroExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return auth, nil
	}
	updated, errRefresh := e.refreshToken(ctx, auth.Clone())
	if errRefresh != nil {
		return nil, errRefresh
	}
	return updated, nil
}

// CountTokens counts tokens for the given request using the Kiro API.
func (e *KiroExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	// Kiro doesn't have a dedicated token counting endpoint
	// Return a basic estimate based on payload size
	tokenCount := len(req.Payload) / 4 // Rough estimate
	result := fmt.Sprintf(`{"total_tokens": %d}`, tokenCount)
	return cliproxyexecutor.Response{Payload: []byte(result)}, nil
}

func (e *KiroExecutor) ensureAccessToken(ctx context.Context, auth *cliproxyauth.Auth) (string, *cliproxyauth.Auth, error) {
	if auth == nil {
		return "", nil, statusErr{code: http.StatusUnauthorized, msg: "missing auth"}
	}
	accessToken := metaStringValue(auth.Metadata, "access_token")
	expiry := kiroTokenExpiry(auth.Metadata)
	if accessToken != "" && expiry.After(time.Now().Add(kiroRefreshSkew)) {
		return accessToken, nil, nil
	}
	updated, errRefresh := e.refreshToken(ctx, auth.Clone())
	if errRefresh != nil {
		return "", nil, errRefresh
	}
	return metaStringValue(updated.Metadata, "access_token"), updated, nil
}

func (e *KiroExecutor) refreshToken(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing auth"}
	}
	refreshToken := metaStringValue(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return auth, statusErr{code: http.StatusUnauthorized, msg: "missing refresh token"}
	}

	authMethod := metaStringValue(auth.Metadata, "auth_method")
	region := e.getRegion(auth)
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)

	var accessToken string
	var newRefreshToken string
	var expiresIn int64 = 3600

	if strings.ToLower(authMethod) == "idc" || strings.ToLower(authMethod) == "builderid" {
		clientID := metaStringValue(auth.Metadata, "client_id")
		clientSecret := metaStringValue(auth.Metadata, "client_secret")
		if clientID == "" || clientSecret == "" {
			return auth, statusErr{code: http.StatusUnauthorized, msg: "missing client credentials for IdC auth"}
		}

		tokenURL := fmt.Sprintf(kiroIdCTokenURL, region)
		data := url.Values{}
		data.Set("grant_type", "refresh_token")
		data.Set("client_id", clientID)
		data.Set("client_secret", clientSecret)
		data.Set("refresh_token", refreshToken)

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
		if err != nil {
			return auth, err
		}
		httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		httpReq.Header.Set("Accept", "application/json")
		httpReq.Header.Set("User-Agent", "aws-sdk-js/3.738.0")

		resp, err := httpClient.Do(httpReq)
		if err != nil {
			return auth, err
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return auth, err
		}

		if resp.StatusCode != http.StatusOK {
			return auth, statusErr{code: resp.StatusCode, msg: string(body)}
		}

		var tokenResp struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int64  `json:"expires_in"`
		}
		if err := json.Unmarshal(body, &tokenResp); err != nil {
			return auth, err
		}

		accessToken = tokenResp.AccessToken
		if tokenResp.RefreshToken != "" {
			newRefreshToken = tokenResp.RefreshToken
		}
		if tokenResp.ExpiresIn > 0 {
			expiresIn = tokenResp.ExpiresIn
		}
	} else {
		// Social auth refresh
		refreshURL := fmt.Sprintf(kiroSocialRefreshURL, region)
		reqBody := map[string]string{"refreshToken": refreshToken}
		jsonBody, err := json.Marshal(reqBody)
		if err != nil {
			return auth, err
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshURL, bytes.NewReader(jsonBody))
		if err != nil {
			return auth, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "application/json")
		httpReq.Header.Set("User-Agent", "aws-sdk-js/1.0.27 ua/2.1 os/windows lang/js md/nodejs#22.21.1 api/codewhispererstreaming#1.0.27")

		resp, err := httpClient.Do(httpReq)
		if err != nil {
			return auth, err
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return auth, err
		}

		if resp.StatusCode != http.StatusOK {
			return auth, statusErr{code: resp.StatusCode, msg: string(body)}
		}

		var tokenResp struct {
			AccessToken string `json:"accessToken"`
			ExpiresIn   int64  `json:"expiresIn"`
		}
		if err := json.Unmarshal(body, &tokenResp); err != nil {
			return auth, err
		}

		accessToken = tokenResp.AccessToken
		if tokenResp.ExpiresIn > 0 {
			expiresIn = tokenResp.ExpiresIn
		}
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = accessToken
	if newRefreshToken != "" {
		auth.Metadata["refresh_token"] = newRefreshToken
	}
	auth.Metadata["timestamp"] = time.Now().UnixMilli()
	auth.Metadata["expired"] = time.Now().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339)
	auth.Metadata["type"] = kiroAuthType
	return auth, nil
}

func (e *KiroExecutor) buildRequest(ctx context.Context, auth *cliproxyauth.Auth, token string, payload []byte, baseURL string) (*http.Request, error) {
	if token == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}

	// Amazon Q API uses REST-style endpoint
	requestURL := strings.TrimSuffix(baseURL, "/") + kiroGenerateAssistantPath
	region := e.getRegion(auth)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}

	// Generate machine ID following kiro.rs logic
	machineID := generateKiroMachineID(auth)
	if machineID == "" {
		machineID = uuid.NewString()[:8] + uuid.NewString()[:8] // fallback
	}

	// Get OS name following kiro.rs format
	osName := getKiroOSName()

	// Build User-Agent and x-amz-user-agent following kiro.rs exactly
	xAmzUserAgent := fmt.Sprintf("aws-sdk-js/1.0.27 KiroIDE-%s-%s", kiroVersion, machineID)
	userAgent := fmt.Sprintf(
		"aws-sdk-js/1.0.27 ua/2.1 os/%s lang/js md/nodejs#%s api/codewhispererstreaming#1.0.27 m/E KiroIDE-%s-%s",
		osName, kiroNodeVersion, kiroVersion, machineID,
	)

	// Amazon Q API headers (following kiro.rs implementation exactly)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-amzn-codewhisperer-optout", "true")
	httpReq.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	httpReq.Header.Set("x-amz-user-agent", xAmzUserAgent)
	httpReq.Header.Set("User-Agent", userAgent)
	httpReq.Header.Set("Host", fmt.Sprintf("q.%s.amazonaws.com", region))
	httpReq.Header.Set("amz-sdk-invocation-id", uuid.NewString())
	httpReq.Header.Set("amz-sdk-request", "attempt=1; max=3")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Connection", "close")

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       requestURL,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      payload,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	return httpReq, nil
}

// getKiroOSName returns OS name in kiro.rs format
func getKiroOSName() string {
	switch runtime.GOOS {
	case "darwin":
		return "darwin#24.6.0"
	case "windows":
		return "win32#10.0.22631"
	case "linux":
		return "linux#6.5.0"
	default:
		return runtime.GOOS
	}
}

func (e *KiroExecutor) applyKiroPayload(payload []byte, model string, auth *cliproxyauth.Auth) []byte {
	// Extract messages from OpenAI-style payload
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	messageArray := messages.Array()
	if len(messageArray) == 0 {
		return payload
	}

	// Map model names to Kiro format (following kiro.rs)
	modelID := kiroMapModel(model)

	// Determine chat trigger type (MANUAL by default, AUTO if tools present)
	chatTriggerType := "MANUAL"
	if gjson.GetBytes(payload, "tools").Exists() {
		toolChoice := gjson.GetBytes(payload, "tool_choice.type").String()
		if toolChoice == "any" || toolChoice == "tool" {
			chatTriggerType = "AUTO"
		}
	}

	// Build history and extract current message
	var history []map[string]any
	var currentContent string
	var systemMessages []string

	for i, msg := range messageArray {
		role := msg.Get("role").String()
		content := msg.Get("content").String()

		if i == len(messageArray)-1 && role == "user" {
			// Last user message becomes the current message
			currentContent = content
		} else if role == "system" {
			// Collect system messages
			systemMessages = append(systemMessages, content)
		} else {
			// Build history entry using camelCase (following kiro.rs format)
			var historyEntry map[string]any
			if role == "assistant" {
				historyEntry = map[string]any{
					"assistantResponseMessage": map[string]any{
						"content": content,
					},
				}
			} else {
				historyEntry = map[string]any{
					"userInputMessage": map[string]any{
						"content": content,
						"modelId": modelID,
						"origin":  "AI_EDITOR",
						"userInputMessageContext": map[string]any{},
					},
				}
			}
			history = append(history, historyEntry)
		}
	}

	// Handle system messages by prepending to history (following kiro.rs pattern)
	if len(systemMessages) > 0 {
		systemContent := strings.Join(systemMessages, "\n")
		systemHistory := []map[string]any{
			{
				"userInputMessage": map[string]any{
					"content": systemContent,
					"modelId": modelID,
					"origin":  "AI_EDITOR",
					"userInputMessageContext": map[string]any{},
				},
			},
			{
				"assistantResponseMessage": map[string]any{
					"content": "I will follow these instructions.",
				},
			},
		}
		history = append(systemHistory, history...)
	}

	// Build userInputMessage with camelCase
	userInputMessage := map[string]any{
		"content": currentContent,
		"modelId": modelID,
		"origin":  "AI_EDITOR",
		"userInputMessageContext": map[string]any{},
	}

	// Build conversationState using camelCase (following kiro.rs format exactly)
	conversationState := map[string]any{
		"conversationId":       uuid.NewString(),
		"agentContinuationId":  uuid.NewString(),
		"agentTaskType":        "vibe",
		"chatTriggerType":      chatTriggerType,
		"currentMessage": map[string]any{
			"userInputMessage": userInputMessage,
		},
		"history": history,
	}

	requestBody := map[string]any{
		"conversationState": conversationState,
	}

	result, err := json.Marshal(requestBody)
	if err != nil {
		return payload
	}
	return result
}

// kiroMapModel maps model names to Kiro format (following kiro.rs)
func kiroMapModel(model string) string {
	modelLower := strings.ToLower(model)
	switch {
	case strings.Contains(modelLower, "sonnet") && strings.Contains(modelLower, "thinking"):
		return "claude-sonnet-4.5-thinking"
	case strings.Contains(modelLower, "sonnet"):
		return "claude-sonnet-4.5"
	case strings.Contains(modelLower, "opus"):
		return "claude-opus-4.5"
	case strings.Contains(modelLower, "haiku"):
		return "claude-haiku-4.5"
	default:
		return "claude-sonnet-4.5"
	}
}

func (e *KiroExecutor) getRegion(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Metadata != nil {
		if region := metaStringValue(auth.Metadata, "region"); region != "" {
			return region
		}
	}
	return kiroDefaultRegion
}

func (e *KiroExecutor) generateUserAgent() string {
	os := runtime.GOOS
	arch := runtime.GOARCH
	return fmt.Sprintf("KiroIDE/1.0.0 %s/%s cli-proxy-api", os, arch)
}

func (e *KiroExecutor) generateXAmzUserAgent() string {
	// Following kiro.rs format: aws-sdk-js/1.0.27 KiroIDE-{version}-{machine_id}
	machineID := uuid.NewString()[:8]
	return fmt.Sprintf("aws-sdk-js/1.0.27 KiroIDE-1.0.0-%s", machineID)
}

func (e *KiroExecutor) generateMachineID() string {
	// Generate a stable machine ID for anti-detection
	return fmt.Sprintf("cli-proxy-api-%s", uuid.NewString()[:8])
}

// generateKiroMachineID generates a machine ID following kiro.rs logic:
// SHA256("KotlinNativeAPI/{profileArn}") or SHA256("KotlinNativeAPI/{refreshToken}")
func generateKiroMachineID(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}

	// Try profile_arn first
	if profileArn, ok := auth.Metadata["profile_arn"].(string); ok && profileArn != "" {
		if strings.HasPrefix(profileArn, "arn:aws") && strings.Contains(profileArn, "profile/") {
			hash := sha256.Sum256([]byte("KotlinNativeAPI/" + profileArn))
			return hex.EncodeToString(hash[:])
		}
	}

	// Fall back to refresh_token
	if refreshToken, ok := auth.Metadata["refresh_token"].(string); ok && refreshToken != "" {
		hash := sha256.Sum256([]byte("KotlinNativeAPI/" + refreshToken))
		return hex.EncodeToString(hash[:])
	}

	return ""
}

func kiroTokenExpiry(metadata map[string]any) time.Time {
	if metadata == nil {
		return time.Time{}
	}
	if expStr, ok := metadata["expired"].(string); ok {
		expStr = strings.TrimSpace(expStr)
		if expStr != "" {
			if parsed, errParse := time.Parse(time.RFC3339, expStr); errParse == nil {
				return parsed
			}
		}
	}
	expiresIn, hasExpires := int64Value(metadata["expires_in"])
	tsMs, hasTimestamp := int64Value(metadata["timestamp"])
	if hasExpires && hasTimestamp {
		return time.Unix(0, tsMs*int64(time.Millisecond)).Add(time.Duration(expiresIn) * time.Second)
	}
	return time.Time{}
}

func parseKiroUsage(body []byte) usage.Detail {
	usageNode := gjson.GetBytes(body, "usage")
	if !usageNode.Exists() {
		return usage.Detail{}
	}
	return usage.Detail{
		InputTokens:  usageNode.Get("input_tokens").Int(),
		OutputTokens: usageNode.Get("output_tokens").Int(),
	}
}

func parseKiroStreamUsage(payload []byte) (usage.Detail, bool) {
	usageNode := gjson.GetBytes(payload, "usage")
	if !usageNode.Exists() {
		return usage.Detail{}, false
	}
	return usage.Detail{
		InputTokens:  usageNode.Get("input_tokens").Int(),
		OutputTokens: usageNode.Get("output_tokens").Int(),
	}, true
}

// FetchKiroModels retrieves available models for Kiro.
// Since Kiro uses Claude models, we return a predefined list.
func FetchKiroModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) []*registry.ModelInfo {
	now := time.Now().Unix()
	return []*registry.ModelInfo{
		{
			ID:          "claude-sonnet-4",
			Name:        "claude-sonnet-4",
			Description: "Claude Sonnet 4 via Kiro",
			DisplayName: "Claude Sonnet 4 (Kiro)",
			Version:     "claude-sonnet-4",
			Object:      "model",
			Created:     now,
			OwnedBy:     kiroAuthType,
			Type:        kiroAuthType,
		},
		{
			ID:          "claude-sonnet-4-20250514",
			Name:        "claude-sonnet-4-20250514",
			Description: "Claude Sonnet 4 (20250514) via Kiro",
			DisplayName: "Claude Sonnet 4 20250514 (Kiro)",
			Version:     "claude-sonnet-4-20250514",
			Object:      "model",
			Created:     now,
			OwnedBy:     kiroAuthType,
			Type:        kiroAuthType,
		},
	}
}
