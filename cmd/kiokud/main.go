package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mash/kioku/internal/auth"
	"github.com/mash/kioku/internal/blob"
	"github.com/mash/kioku/internal/config"
	"github.com/mash/kioku/internal/server"
	"github.com/mash/kioku/internal/store"
	"github.com/mash/kioku/internal/usd"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	addr := flag.String("addr", env("KIOKU_ADDR", "127.0.0.1:8080"), "listen address")
	dir := flag.String("data", env("KIOKU_DATA", "./data"), "persistent metadata directory")
	onlineBackup := flag.String("online-backup", "", "request an online configuration backup from the running server")
	adminSocket := flag.String("admin-socket", "", "operator Unix socket (default DATA/admin.sock)")
	backup := flag.String("backup", "", "write a configuration-only SQLite backup, then exit (offline operator command)")
	maxSources := flag.Int("max-sources", 200, "maximum sources per project")
	maxBytes := flag.Int64("max-source-bytes", 64<<20, "maximum original and extracted bytes per project")
	maxJobs := flag.Int("max-jobs", 20, "maximum queued/running generation jobs per project")
	generateEvery := flag.Duration("generate-every", 24*time.Hour, "periodic generation interval for changed projects; 0 disables scheduling")
	intakeWorkers := flag.Int("intake-workers", 4, "concurrent URL fetch workers")
	intakeJobs := flag.Int("max-intake-jobs", 200, "maximum queued/running URL puts per project")
	rate := flag.Int("rate-per-minute", 600, "maximum requests per source IP per minute")
	agentMaxSessions := flag.Int("agent-max-sessions", 4, "maximum concurrent managed agent sessions")
	agentJobTimeout := flag.Duration("agent-job-timeout", time.Hour, "managed generation deadline")
	agentMaxUSD := flag.String("agent-max-session-usd", "10", "maximum provider list cost per managed session in USD")
	trustedProxy := flag.String("trusted-proxy", "", "CIDR of the proxy allowed to supply X-Forwarded-For")
	flag.Parse()
	var trustedNetwork *net.IPNet
	if *trustedProxy != "" {
		_, network, parseErr := net.ParseCIDR(*trustedProxy)
		if parseErr != nil {
			return errors.New("trusted-proxy must be a CIDR")
		}
		trustedNetwork = network
	}
	agentMaxMicros, err := usd.Parse(*agentMaxUSD)
	if err != nil || agentMaxMicros < 500_000 || agentMaxMicros%10000 != 0 || *agentMaxSessions < 1 || *agentJobTimeout <= 0 {
		return errors.New("invalid managed agent limits; use a whole-cent budget of at least $0.50")
	}
	agentVersion, err := strconv.ParseInt(env("KIOKU_MA_AGENT_VERSION", "0"), 10, 64)
	if err != nil || agentVersion < 0 {
		return errors.New("KIOKU_MA_AGENT_VERSION must be a nonnegative integer")
	}
	if *adminSocket == "" {
		*adminSocket = filepath.Join(*dir, "admin.sock")
	}
	if *onlineBackup != "" {
		return requestOnlineBackup(*adminSocket, *onlineBackup)
	}
	if *backup != "" {
		db, err := store.OpenControl(*dir)
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Backup(*backup)
	}
	if *maxSources < 1 || *maxBytes < 1 || *maxJobs < 1 || *rate < 1 || *intakeWorkers < 1 || *intakeJobs < 1 || *generateEvery < 0 || *generateEvery > 0 && *generateEvery < time.Second {
		return errors.New("limits must be positive; generation interval must be zero or at least one second")
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
	if err = os.MkdirAll(*dir, 0700); err != nil {
		return err
	}
	key := config.Env("KIOKU_ENCRYPTION_KEY", "")
	cipher, err := auth.NewCipher(key)
	if err != nil {
		return err
	}
	var objects blob.Store = blob.Local{Dir: filepath.Join(*dir, "objects")}
	if os.Getenv("S3_ENDPOINT") != "" {
		return errors.New("S3_ENDPOINT is no longer supported; configure R2_ACCOUNT_ID and R2 credentials")
	}
	if account := os.Getenv("R2_ACCOUNT_ID"); account != "" {
		remote, err := blob.NewR2Projects(account, os.Getenv("R2_BUCKET"), os.Getenv("R2_ACCESS_KEY_ID"), os.Getenv("R2_SECRET_ACCESS_KEY"))
		if err != nil {
			return err
		}
		objects = &blob.Cache{Remote: remote, Dir: filepath.Join(*dir, "cache"), MaxBytes: 256 << 20}
	} else if os.Getenv("R2_BUCKET") != "" || os.Getenv("R2_ACCESS_KEY_ID") != "" || os.Getenv("R2_SECRET_ACCESS_KEY") != "" {
		return errors.New("R2_ACCOUNT_ID is required when R2 credentials or bucket are configured")
	}

	db, err := store.OpenWithObjects(*dir, objects)
	if err != nil {
		return err
	}
	defer db.Close()
	if cache, ok := objects.(*blob.Cache); ok {
		if err = cache.PruneUnused(); err != nil {
			return err
		}
	}
	providers := map[string]auth.Provider{}
	for _, name := range []string{"google", "github"} {
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
	app := server.New(server.Config{TrustedProxy: trustedNetwork, AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"), MAAgentID: os.Getenv("KIOKU_MA_AGENT_ID"), MAAgentVersion: agentVersion, MAEnvironmentID: os.Getenv("KIOKU_MA_ENVIRONMENT_ID"), MABaseURL: env("KIOKU_MA_BASE_URL", "https://api.anthropic.com"), AnthropicWebhookSigningKey: os.Getenv("ANTHROPIC_WEBHOOK_SIGNING_KEY"), AgentMaxSessions: *agentMaxSessions, AgentJobTimeout: *agentJobTimeout, AgentMaxSessionCents: agentMaxMicros / 10000, GenerateEvery: *generateEvery, IntakeWorkers: *intakeWorkers, MaxIntakeJobs: *intakeJobs, BaseURL: base, Providers: providers, MaxSources: *maxSources, MaxBytes: *maxBytes, MaxQueued: *maxJobs, RatePerMinute: *rate, MetricsToken: config.Env("KIOKU_METRICS_TOKEN", "")}, db, objects, cipher)
	if err = app.Prepare(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stopAdmin, err := serveAdmin(ctx, *adminSocket, db)
	if err != nil {
		return err
	}
	defer stopAdmin()
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
		slog.Info("kioku server ready", "url", base, "storage", map[bool]string{true: "R2", false: "local"}[os.Getenv("R2_ACCOUNT_ID") != ""])
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
func env(key, fallback string) string {
	return config.Env(key, fallback)
}
