// Command stashvent-server is the StashVent API server.
//
// It serves the /v1 share endpoints (create, complete, get, delete) and the
// health endpoints /healthz and /readyz; see the stashvent-api skill. On
// startup it applies the embedded migrations, then runs the expiry sweep in
// the background.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/storage"
	"github.com/stashvent/stashvent/internal/storage/postgres"
)

// version is set at build time: -ldflags "-X main.version=<version>".
var version = "dev"

const shutdownTimeout = 10 * time.Second

type config struct {
	addr           string
	logLevel       slog.Level
	databaseURL    string
	s3             storage.S3Config
	maxPayload     int64
	limits         rateLimitConfig
	trustedProxies []netip.Prefix
	abuse          abuseConfig
}

// abuseConfig holds the controls on who may push and how much.
// Zero values turn the caps off, not on.
type abuseConfig struct {
	requireAPIKey bool  // POST /v1/shares needs an API key
	sharesPerDay  int   // per key and UTC day; 0: no quota
	maxStored     int64 // declared bytes of all unexpired shares; 0: no cap
	maxPulls      int   // lookups per share; 0: no cap
}

func main() {
	if err := serverCommand().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "stashvent-server: %v\n", err)
		os.Exit(1)
	}
}

// openDatabase connects to Postgres and applies the embedded migrations.
func openDatabase(ctx context.Context, databaseURL string, logger *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	// Logged first: Migrate waits (up to 5 minutes) if another replica holds
	// the migration lock, and nothing is listening until it returns.
	logger.Info("applying migrations")
	schemaVersion, err := postgres.Migrate(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	logger.Info("migrations applied", slog.Int64("schema_version", schemaVersion))
	return pool, nil
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	logger := slog.New(requestIDHandler{slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.logLevel})})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := openDatabase(ctx, cfg.databaseURL, logger)
	if err != nil {
		return err
	}
	defer pool.Close()

	store := postgres.New(pool)
	objects := storage.NewObjects(cfg.s3)

	h := &handler{
		store:      store,
		keys:       store,
		objects:    objects,
		maxPayload: cfg.maxPayload,
		abuse:      cfg.abuse,
		logger:     logger,
		now:        time.Now,
		limits:     newRateLimits(cfg.limits, cfg.trustedProxies, time.Now),
		ready: &readiness{checks: []readyCheck{
			{name: "postgres", ping: pool.Ping},
			{name: "storage", ping: objects.Ping},
		}},
	}
	// Logged so a wrong limit or proxy list shows at startup: with a proxy in
	// front and none trusted, every client shares the proxy's bucket.
	logger.Info("rate limits per client",
		limitAttr("create", cfg.limits.create),
		limitAttr("lookup", cfg.limits.lookup),
		limitAttr("other", cfg.limits.other),
		limitAttr("key_create", cfg.limits.keyCreate),
		slog.Any("trusted_proxies", cfg.trustedProxies))
	logAbuseControls(logger, cfg.abuse)

	// Stopped by ctx; run waits for it so a sweep never outlives the pool.
	sw := &sweeper{store: store, objects: objects, logger: logger}
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		sw.run(ctx)
	}()
	defer func() {
		stop()
		<-sweepDone
	}()

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           newMux(h),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// Longer than the slowest handler: DELETE may use the whole request
		// deadline on the object and then revokeTimeout on the row. A shorter
		// value would drop the connection on a delete that succeeded.
		WriteTimeout: requestTimeout + revokeTimeout + 5*time.Second,
		IdleTimeout:  60 * time.Second,
		ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("server starting", slog.String("addr", cfg.addr), slog.String("version", version))
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("listening on %s: %w", cfg.addr, err)
		}
		return nil
	case <-ctx.Done():
	}

	logger.Info("server shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	return nil
}

func loadConfig() (config, error) {
	cfg := config{
		addr:     envOr("SERVER_ADDR", ":8080"),
		logLevel: slog.LevelInfo,
		s3: storage.S3Config{
			Endpoint:       os.Getenv("S3_ENDPOINT"),
			PublicEndpoint: os.Getenv("S3_PUBLIC_ENDPOINT"),
			Region:         envOr("S3_REGION", "us-east-1"),
			Bucket:         os.Getenv("S3_BUCKET"),
			UsePathStyle:   envOr("S3_USE_PATH_STYLE", "false") == "true",
			AccessKey:      os.Getenv("S3_ACCESS_KEY"),
			SecretKey:      os.Getenv("S3_SECRET_KEY"),
		},
		databaseURL: os.Getenv("DATABASE_URL"),
		maxPayload:  api.DefaultMaxPayloadBytes,
	}

	if lvl := os.Getenv("LOG_LEVEL"); lvl != "" {
		if err := cfg.logLevel.UnmarshalText([]byte(lvl)); err != nil {
			return config{}, fmt.Errorf("invalid LOG_LEVEL %q: %w", lvl, err)
		}
	}
	if v := os.Getenv("MAX_PAYLOAD_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return config{}, fmt.Errorf("invalid MAX_PAYLOAD_BYTES %q", v)
		}
		// The CLI never sends or downloads more, so a higher limit would only
		// let other clients store shares no CLI can pull.
		if n > api.DefaultMaxPayloadBytes {
			return config{}, fmt.Errorf("MAX_PAYLOAD_BYTES %d is above %d (%d MB), the most the CLI sends", n, api.DefaultMaxPayloadBytes, api.DefaultMaxPayloadBytes>>20)
		}
		cfg.maxPayload = n
	}

	limits := []struct {
		name     string
		limit    *rateLimit
		fallback rateLimit
	}{
		{"CREATE", &cfg.limits.create, rateLimit{perMinute: 10, burst: 5}},
		{"LOOKUP", &cfg.limits.lookup, rateLimit{perMinute: 30, burst: 10}},
		{"OTHER", &cfg.limits.other, rateLimit{perMinute: 60, burst: 20}},
		{"KEY_CREATE", &cfg.limits.keyCreate, rateLimit{perMinute: 10, burst: 5}},
	}
	for _, l := range limits {
		var err error
		if l.limit.perMinute, err = envPositiveInt("RATE_LIMIT_"+l.name+"_PER_MINUTE", l.fallback.perMinute); err != nil {
			return config{}, err
		}
		if l.limit.burst, err = envPositiveInt("RATE_LIMIT_"+l.name+"_BURST", l.fallback.burst); err != nil {
			return config{}, err
		}
	}
	abuse, err := loadAbuseConfig()
	if err != nil {
		return config{}, err
	}
	cfg.abuse = abuse
	trusted, err := parseTrustedProxies(os.Getenv("TRUSTED_PROXIES"))
	if err != nil {
		return config{}, fmt.Errorf("invalid TRUSTED_PROXIES: %w", err)
	}
	cfg.trustedProxies = trusted

	required := map[string]string{
		"DATABASE_URL":       cfg.databaseURL,
		"S3_ENDPOINT":        cfg.s3.Endpoint,
		"S3_PUBLIC_ENDPOINT": cfg.s3.PublicEndpoint,
		"S3_BUCKET":          cfg.s3.Bucket,
		"S3_ACCESS_KEY":      cfg.s3.AccessKey,
		"S3_SECRET_KEY":      cfg.s3.SecretKey,
	}
	for name, v := range required {
		if v == "" {
			return config{}, fmt.Errorf("%s is required", name)
		}
	}
	return cfg, nil
}

// loadAbuseConfig reads the API key switch and the caps. A value that can't be
// used stops the server, like the rate limits: running with no cap, or a
// different one, by accident is worse than not starting.
func loadAbuseConfig() (abuseConfig, error) {
	cfg := abuseConfig{}
	if v := os.Getenv("REQUIRE_API_KEY"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return abuseConfig{}, fmt.Errorf("invalid REQUIRE_API_KEY %q: want true or false", v)
		}
		cfg.requireAPIKey = b
	}
	var err error
	if cfg.sharesPerDay, err = envNonNegativeInt("API_KEY_SHARES_PER_DAY", 10); err != nil {
		return abuseConfig{}, err
	}
	if cfg.maxPulls, err = envNonNegativeInt("MAX_PULLS_PER_SHARE", 50); err != nil {
		return abuseConfig{}, err
	}
	if v := os.Getenv("MAX_STORED_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return abuseConfig{}, fmt.Errorf("invalid MAX_STORED_BYTES %q", v)
		}
		cfg.maxStored = n
	}
	return cfg, nil
}

// logAbuseControls says at startup what is in force, so a self-hoster sees it.
func logAbuseControls(logger *slog.Logger, cfg abuseConfig) {
	quota := fmt.Sprintf("%d shares/day per key", cfg.sharesPerDay)
	if cfg.sharesPerDay == 0 {
		quota = "no daily quota"
	}
	stored := strconv.FormatInt(cfg.maxStored, 10) + " bytes"
	if cfg.maxStored == 0 {
		stored = "no cap"
	}
	pulls := strconv.Itoa(cfg.maxPulls)
	if cfg.maxPulls == 0 {
		pulls = "no cap"
	}
	attrs := []any{
		slog.Bool("require_api_key", cfg.requireAPIKey),
		slog.String("max_stored", stored),
		slog.String("max_pulls_per_share", pulls),
	}
	if cfg.requireAPIKey {
		attrs = append(attrs, slog.String("quota", quota))
	}
	logger.Info("abuse controls", attrs...)
	if !cfg.requireAPIKey {
		logger.Warn("REQUIRE_API_KEY is off: anyone who can reach this server can create shares. " +
			"That is fine on a private network; turn API keys on before exposing it to the internet.")
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envPositiveInt reads a whole number above zero, or returns fallback if key
// is not set.
func envPositiveInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid %s %q", key, v)
	}
	return n, nil
}

// envNonNegativeInt reads a whole number of zero or more, or returns fallback
// if key is not set.
func envNonNegativeInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid %s %q", key, v)
	}
	return n, nil
}

func limitAttr(name string, l rateLimit) slog.Attr {
	return slog.Group(name, slog.Int("per_minute", l.perMinute), slog.Int("burst", l.burst))
}

// newMux returns the server's routes behind the request log. Rate limits are
// applied per route, so the health endpoints are exempt by not being wrapped.
func newMux(h *handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(routeHealthz, handleHealthz)
	mux.HandleFunc(routeReadyz, h.readyz)
	mux.Handle("POST /v1/shares", h.limits.create.wrap(h.createShare))
	mux.Handle("GET /v1/shares/{id}", h.limits.lookup.wrap(h.getShare))
	mux.Handle("GET /v1/shares/{id}/summary", h.limits.lookup.wrap(h.getShareSummary))
	mux.Handle("POST /v1/shares/{id}/complete", h.limits.other.wrap(h.completeShare))
	mux.Handle("DELETE /v1/shares/{id}", h.limits.other.wrap(h.deleteShare))
	return h.withRequestLog(mux)
}
