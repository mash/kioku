package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/runners"
	"github.com/mash/kioku/internal/server"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/prompts"
)

func main() {
	if err := run(); err != nil {
		var domain server.InvalidSignupDomain
		if errors.As(err, &domain) {
			fmt.Fprintln(os.Stderr, "kiokud:", domain)
		} else {
			slog.Error("server stopped", "error", err)
		}
		os.Exit(1)
	}
}
func run() error {
	addr := flag.String("addr", env("KIOKU_ADDR", "127.0.0.1:8080"), "listen address")
	storage := flag.String("storage", env("KIOKU_STORAGE", string(blob.BackendLocal)), "object storage backend: local or s3")
	dir := flag.String("data", env("KIOKU_DATA", "./data"), "persistent metadata directory")
	trustedProxy := flag.String("trusted-proxy", "", "CIDR of the proxy allowed to supply X-Forwarded-For")
	flag.Parse()
	signupAllowlist, err := server.ParseSignupAllowlist(os.Getenv("KIOKU_SIGNUP_GOOGLE_HD"))
	if err != nil {
		return err
	}
	var trustedNetwork *net.IPNet
	if *trustedProxy != "" {
		_, network, parseErr := net.ParseCIDR(*trustedProxy)
		if parseErr != nil {
			return errors.New("trusted-proxy must be a CIDR")
		}
		trustedNetwork = network
	}
	agentVersion, err := strconv.ParseInt(env("KIOKU_MA_AGENT_VERSION", "0"), 10, 64)
	if err != nil || agentVersion < 0 {
		return errors.New("KIOKU_MA_AGENT_VERSION must be a nonnegative integer")
	}

	base := strings.TrimRight(env("KIOKU_BASE_URL", "http://127.0.0.1:8080"), "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil || u.Fragment != "" {
		return errors.New("KIOKU_BASE_URL must be an origin")
	}
	loopback := net.ParseIP(u.Hostname())
	local := u.Hostname() == "localhost" || (loopback != nil && loopback.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return errors.New("HTTPS is required outside localhost")
	}
	rendered, err := prompts.Load(prompts.Variables{BaseURL: base, ClientVersion: server.MinimumClientVersion, MaxOutputBytes: 2 * server.MaxWikiBytes}, server.MCPToolNames())
	if err != nil {
		return err
	}
	rendered.Log()
	if err = os.MkdirAll(*dir, 0700); err != nil {
		return err
	}
	objects, err := openStorage(blob.Backend(*storage), *dir, os.Getenv)
	if err != nil {
		return err
	}

	stateStore, cipher, err := store.OpenStorage(context.Background(), *dir, objects, func(create bool) (*auth.Cipher, error) {
		return loadCipherForStorage(*dir, os.Getenv("KIOKU_ENCRYPTION_KEY"), create)
	})
	if err != nil {
		return err
	}
	defer stateStore.Close()
	providers := map[string]auth.Provider{}
	for _, name := range auth.ProviderNames() {
		prefix := strings.ToUpper(name)
		id, secret := os.Getenv(prefix+"_CLIENT_ID"), os.Getenv(prefix+"_CLIENT_SECRET")
		if id != "" || secret != "" {
			if id == "" || secret == "" {
				return errors.New(prefix + "_CLIENT_ID and " + prefix + "_CLIENT_SECRET are both required")
			}
			providers[name] = auth.Provider{Name: name, ClientID: id, ClientSecret: secret, RedirectURL: base + "/auth/" + name + "/callback"}
		}
	}
	if len(providers) == 0 {
		return errors.New("configure at least one Google or GitHub OAuth provider")
	}
	selection := os.Getenv("KIOKU_RUNNER")
	command, err := runners.ParseCommand(selection, os.Getenv("KIOKU_RUNNER_COMMAND"))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app := server.New(server.Config{
		Prompts:                    rendered,
		TrustedProxy:               trustedNetwork,
		AnthropicWebhookSigningKey: os.Getenv("ANTHROPIC_WEBHOOK_SIGNING_KEY"),
		GenerateDebounce:           10 * time.Second,
		BaseURL:                    base,
		Providers:                  providers,
		SignupAllowlist:            signupAllowlist,
		MetricsToken:               os.Getenv("KIOKU_METRICS_TOKEN"),
	}, stateStore, objects, cipher)
	set, err := runners.Load(runners.Config{
		Prompts:         rendered,
		Type:            selection,
		Command:         command,
		DataDir:         *dir,
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"),
		MAAgentID:       os.Getenv("KIOKU_MA_AGENT_ID"),
		MAAgentVersion:  agentVersion,
		MAEnvironmentID: os.Getenv("KIOKU_MA_ENVIRONMENT_ID"),
		MABaseURL:       env("KIOKU_MA_BASE_URL", "https://api.anthropic.com"),
		BaseURL:         base,
	}, func(jobID string) { app.ReconcileJob(ctx, jobID) })
	if err != nil {
		return err
	}
	defer func() {
		stop()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := set.Close(ctx); err != nil {
			slog.Error("runner shutdown incomplete", "error", err)
		}
	}()

	app.SetRunner(set.Runner)
	if err = app.Prepare(); err != nil {
		return err
	}
	if err = app.EnsureGeneralWikis(ctx); err != nil {
		return fmt.Errorf("provision General wikis: %w", err)
	}
	workerDone := make(chan error, 1)
	go func() {
		err := app.RunWorker(ctx)
		workerDone <- err
		if err != nil {
			stop()
		}
	}()
	httpServer := &http.Server{Addr: *addr, Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 6 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	serverDone := make(chan error, 1)
	go func() {
		slog.Info("kioku server ready", "url", base, "storage", *storage)
		serverDone <- httpServer.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
	case err = <-serverDone:
		stop()
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = httpServer.Shutdown(shutdown)
	stop()
	workerErr := <-workerDone
	if err != nil {
		return err
	}
	return workerErr
}

// env returns a nonempty environment setting or the fallback.
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
