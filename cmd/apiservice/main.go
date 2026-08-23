// Command apiservice runs the REST API Service (design doc §4.1): the only
// public-facing surface. Stateless — no Redis dependency, no direct access
// to the Controller's data. Every read and every invocation goes through
// the Controller's API.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agentbox/internal/apiservice"
	"agentbox/internal/config"
	"agentbox/internal/logging"
)

// Config holds every environment-derived setting the REST API Service
// needs, loaded once in loadConfig rather than scattered across main().
type Config struct {
	ListenAddr         string
	ControllerURL      string
	APIKey             config.Secret
	RoutingTokenSecret config.Secret
	GuestProxyTimeout  time.Duration
	LogLevel           string
	LogFormat          string
}

func loadConfig() (Config, error) {
	var cfg Config
	var err error

	cfg.ListenAddr = config.String("LISTEN_ADDR", ":8080")
	cfg.ControllerURL = config.String("CONTROLLER_URL", "http://localhost:9090")
	cfg.LogLevel = config.String("LOG_LEVEL", "info")
	cfg.LogFormat = config.String("LOG_FORMAT", "text")

	// API_KEY (§8: single static key for the prototype) and
	// ROUTING_TOKEN_SECRET are both credentials with no safe default —
	// a hardcoded fallback for either would be a real vulnerability, not
	// just a convenience. Both are required, and both are Secrets so they
	// can't accidentally end up in a log line.
	if cfg.APIKey, err = config.RequiredSecret("API_KEY"); err != nil {
		return cfg, fmt.Errorf("load config: %w (§8: single static key for the prototype)", err)
	}
	if cfg.RoutingTokenSecret, err = config.RequiredSecret("ROUTING_TOKEN_SECRET"); err != nil {
		return cfg, fmt.Errorf("load config: %w (must match the Controller's ROUTING_TOKEN_SECRET, §4.2 routing token contract)", err)
	}
	if cfg.GuestProxyTimeout, err = config.Duration("GUEST_PROXY_TIMEOUT", 30*time.Second); err != nil {
		return cfg, fmt.Errorf("load config: %w", err)
	}
	return cfg, nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "apiservice: "+err.Error())
		os.Exit(1)
	}
	logger := logging.New(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)

	ctrl := apiservice.NewHTTPControllerClient(cfg.ControllerURL)
	tokens := apiservice.NewTokenVerifier([]byte(cfg.RoutingTokenSecret.Value()))
	proxy := apiservice.NewHTTPGuestProxy(cfg.GuestProxyTimeout)
	svc := apiservice.NewService(ctrl, tokens, proxy)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: logging.HTTPMiddleware(logger, apiservice.NewRouter(svc, cfg.APIKey.Value()))}
	go func() {
		logger.Info("api service listening", "addr", cfg.ListenAddr, "controller_url", cfg.ControllerURL)
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
