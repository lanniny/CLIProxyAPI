package cmd

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	log "github.com/sirupsen/logrus"
)

// DoKiroLogin triggers the Kiro credential import flow through the shared authentication manager.
// Since Kiro uses browser-based OAuth through the Kiro IDE, this function guides users
// to import their existing credentials from the kiro-auth-token.json file.
//
// Parameters:
//   - cfg: The application configuration
//   - options: Login options including browser behavior and prompts
func DoKiroLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}

	promptFn := options.Prompt
	if promptFn == nil {
		promptFn = defaultProjectPrompt()
	}

	manager := newAuthManager()
	authOpts := &sdkAuth.LoginOptions{
		NoBrowser: options.NoBrowser,
		Metadata:  map[string]string{},
		Prompt:    promptFn,
	}

	record, savedPath, err := manager.Login(context.Background(), "kiro", cfg, authOpts)
	if err != nil {
		log.Errorf("Kiro authentication failed: %v", err)
		return
	}

	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if record != nil && record.Label != "" {
		fmt.Printf("Authenticated as %s\n", record.Label)
	}
	fmt.Println("Kiro authentication successful!")
}
