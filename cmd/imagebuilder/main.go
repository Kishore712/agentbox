// Command imagebuilder runs the Image Builder as its own systemd-managed
// service (design doc §4.6), separate from the Controller process that
// calls it. Split out because Build() needs real mount/loop-device
// privileges that turned out to be genuinely unsafe to grant a container
// sharing the host's /dev (see controller.Dockerfile's history) — running
// it natively sidesteps that entirely, the same reasoning behind the Host
// Agent never being containerized. Only functional on Linux with Docker and
// mount privileges — the HTTP layer runs anywhere, but PullImage/MountExt4
// etc. will fail off-target.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"agentbox/internal/config"
	"agentbox/internal/imagebuilder"
	"agentbox/internal/logging"
)

type Config struct {
	ListenAddr        string
	DataDir           string
	MinSizeMiB        int
	MaxSizeMiB        int
	SizeMarginPercent float64
	LogLevel          string
	LogFormat         string
}

func loadConfig() (Config, error) {
	var cfg Config
	var err error

	def := imagebuilder.DefaultConfig()
	cfg.ListenAddr = config.String("LISTEN_ADDR", ":9091")
	cfg.DataDir = config.String("DATA_DIR", def.DataDir)
	cfg.LogLevel = config.String("LOG_LEVEL", "info")
	cfg.LogFormat = config.String("LOG_FORMAT", "text")

	if cfg.MinSizeMiB, err = config.Int("MIN_SIZE_MIB", def.MinSizeMiB); err != nil {
		return cfg, fmt.Errorf("load config: %w", err)
	}
	if cfg.MaxSizeMiB, err = config.Int("MAX_SIZE_MIB", def.MaxSizeMiB); err != nil {
		return cfg, fmt.Errorf("load config: %w", err)
	}
	cfg.SizeMarginPercent = def.SizeMarginPercent
	if v := os.Getenv("SIZE_MARGIN_PERCENT"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("load config: parse SIZE_MARGIN_PERCENT: %w", err)
		}
		cfg.SizeMarginPercent = f
	}
	return cfg, nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "imagebuilder: "+err.Error())
		os.Exit(1)
	}
	logger := logging.New(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)

	ibCfg := imagebuilder.Config{
		DataDir:           cfg.DataDir,
		MinSizeMiB:        cfg.MinSizeMiB,
		MaxSizeMiB:        cfg.MaxSizeMiB,
		SizeMarginPercent: cfg.SizeMarginPercent,
	}
	b := imagebuilder.NewBuilder(imagebuilder.CLIDockerOps{}, imagebuilder.LinuxFilesystemOps{}, ibCfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: logging.HTTPMiddleware(logger, imagebuilder.NewRouter(b))}
	go func() {
		logger.Info("image builder listening", "addr", cfg.ListenAddr, "data_dir", cfg.DataDir)
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
