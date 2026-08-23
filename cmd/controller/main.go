// Command controller runs the Controller service (design doc §4.2): the
// generic compute-provisioning layer that owns Workload registration and
// the Instance lifecycle state machine. It is the sole owner of its Redis
// data store — the REST API Service never connects to it directly (§4.4).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"agentbox/internal/common"
	"agentbox/internal/config"
	"agentbox/internal/controller"
	"agentbox/internal/imagebuilder"
	"agentbox/internal/logging"
)

// Config holds every environment-derived setting the Controller needs.
// Loading it is a single step (loadConfig), separate from wiring
// dependencies — main() shouldn't interleave os.Getenv calls with
// constructing the service.
type Config struct {
	RedisAddr          string
	ListenAddr         string
	RoutingTokenSecret config.Secret
	IdleReaperInterval time.Duration
	DataDir            string
	HostAgents         string // raw "host-1=addr1,host-2=addr2"; parsed by seedHostsFromEnv
	LogLevel           string
	LogFormat          string
}

func loadConfig() (Config, error) {
	var cfg Config
	var err error

	cfg.RedisAddr = config.String("REDIS_ADDR", "localhost:6379")
	cfg.ListenAddr = config.String("LISTEN_ADDR", ":9090")
	cfg.DataDir = config.String("DATA_DIR", "/data")
	cfg.HostAgents = config.String("HOST_AGENTS", "")
	cfg.LogLevel = config.String("LOG_LEVEL", "info")
	cfg.LogFormat = config.String("LOG_FORMAT", "text")

	// ROUTING_TOKEN_SECRET has no safe default: the Controller and REST API
	// Service sign/verify routing tokens with this shared HMAC secret
	// (§4.2), so a guessable or hardcoded fallback would let anyone forge
	// a routing token. It must be provided explicitly.
	if cfg.RoutingTokenSecret, err = config.RequiredSecret("ROUTING_TOKEN_SECRET"); err != nil {
		return cfg, fmt.Errorf("load config: %w (must match the REST API Service's ROUTING_TOKEN_SECRET, §4.2 routing token contract)", err)
	}
	if cfg.IdleReaperInterval, err = config.Duration("IDLE_REAPER_INTERVAL", 30*time.Second); err != nil {
		return cfg, fmt.Errorf("load config: %w", err)
	}
	return cfg, nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		// The structured logger depends on cfg.LogLevel/LogFormat, which
		// may be exactly what failed to load — fall back to a plain
		// stderr write rather than risk a second, more confusing failure.
		fmt.Fprintln(os.Stderr, "controller: "+err.Error())
		os.Exit(1)
	}
	logger := logging.New(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)

	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := rdb.Ping(ctx).Err(); err != nil {
		logger.Error("cannot reach redis", "addr", cfg.RedisAddr, "error", err)
		os.Exit(1)
	}

	store := controller.NewStore(rdb)
	tokens := controller.NewTokenIssuer([]byte(cfg.RoutingTokenSecret.Value()))
	ha := controller.NewHTTPHostAgentClient()

	// Image Builder (§4.6, Phase 2): real Docker/ext4 pipeline. Only
	// functional where Docker and Linux mount privileges actually exist —
	// not this dev machine, but the orchestration is fully unit-tested
	// against fakes (internal/imagebuilder).
	ibCfg := imagebuilder.DefaultConfig()
	ibCfg.DataDir = cfg.DataDir
	ib := imagebuilder.NewBuilder(imagebuilder.CLIDockerOps{}, imagebuilder.LinuxFilesystemOps{}, ibCfg)

	svc := controller.NewService(store, ha, tokens, ib)

	if err := seedHostsFromEnv(ctx, store, logger, cfg.HostAgents); err != nil {
		logger.Error("failed to seed host registry from HOST_AGENTS", "error", err)
		os.Exit(1)
	}

	reaper := controller.NewIdleReaper(svc, cfg.IdleReaperInterval)
	go reaper.Run(ctx)

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: logging.HTTPMiddleware(logger, controller.NewRouter(svc))}
	go func() {
		logger.Info("controller listening", "addr", cfg.ListenAddr, "redis_addr", cfg.RedisAddr, "idle_reaper_interval", cfg.IdleReaperInterval)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown did not complete cleanly", "error", err)
	}
}

// seedHostsFromEnv registers the static Host Agent list (§4.2: "seeded from
// static config at startup, no dynamic host auto-registration in v1").
// raw format: "host-1=10.0.1.5:9000,host-2=10.0.1.6:9000"
func seedHostsFromEnv(ctx context.Context, store *controller.Store, logger *slog.Logger, raw string) error {
	if raw == "" {
		logger.Warn("HOST_AGENTS not set — starting with no registered hosts; CreateInstance will fail until hosts are registered")
		return nil
	}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			logger.Warn("skipping malformed HOST_AGENTS entry — expected host_id=address", "entry", pair)
			continue
		}
		hostID, addr := parts[0], parts[1]
		if hostID == "" || addr == "" {
			logger.Warn("skipping malformed HOST_AGENTS entry — host_id and address must both be non-empty", "entry", pair)
			continue
		}
		if err := store.UpsertHost(ctx, &common.Host{
			HostID:        hostID,
			InternalAddr:  addr,
			Status:        common.HostHealthy,
			LastHeartbeat: time.Now().Unix(),
		}); err != nil {
			return fmt.Errorf("register host %q at %q: %w", hostID, addr, err)
		}
		logger.Info("registered host", "host_id", hostID, "addr", addr)
	}
	return nil
}
