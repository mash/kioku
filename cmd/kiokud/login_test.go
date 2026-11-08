package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/mash/kioku/internal/githubapp"
)

func TestLoginProvidersUseGitHubAppConfiguration(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, enabled          string
		app, google, wantError bool
	}{
		{name: "unconfigured", wantError: true},
		{name: "google only", google: true},
		{name: "app with sources disabled", app: true, enabled: "false"},
		{name: "app with sources enabled", app: true, enabled: "true"},
		{name: "both providers", app: true, google: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := map[string]string{}
			if tc.app {
				settings = map[string]string{"GITHUB_APP_ID": "7", "GITHUB_APP_CLIENT_ID": "Iv.fixture", "GITHUB_APP_CLIENT_SECRET": "app-secret", "GITHUB_APP_SLUG": "test-app", "GITHUB_APP_PRIVATE_KEY_FILE": path, "GITHUB_APP_ENABLED": tc.enabled}
			}
			if tc.google {
				settings["GOOGLE_CLIENT_ID"], settings["GOOGLE_CLIENT_SECRET"] = "google-id", "google-secret"
			}
			get := func(k string) string { return settings[k] }
			app, err := githubapp.LoadConfig(get)
			if err != nil {
				t.Fatal(err)
			}
			providers, err := loginProviders("https://kioku.example", app, get)
			if (err != nil) != tc.wantError {
				t.Fatalf("provider configuration: %v", err)
			}
			github, exists := providers["github"]
			if exists != tc.app || exists && (github.ClientID != app.ClientID || github.ClientSecret != string(app.ClientSecret) || github.RedirectURL != "https://kioku.example/auth/github/callback") {
				t.Fatal("GitHub login did not follow the App configuration")
			}
			google, exists := providers["google"]
			if exists != tc.google || exists && (google.ClientID != "google-id" || google.ClientSecret != "google-secret" || google.RedirectURL != "https://kioku.example/auth/google/callback") {
				t.Fatal("Google login configuration changed")
			}
		})
	}
	for _, key := range []string{"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"} {
		if _, err := loginProviders("https://kioku.example", githubapp.Config{}, func(k string) string {
			if k == key {
				return "partial"
			}
			return ""
		}); err == nil {
			t.Fatal("accepted incomplete Google credentials")
		}
	}
}
