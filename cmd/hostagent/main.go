// Command hostagent runs the Host Agent service (design doc §4.3): the
// systemd service on each KVM-enabled GCE host that actually touches
// Firecracker. Only functional on Linux with KVM, the `firecracker`
// binary, and root privileges for TAP/iptables — everything here compiles
// and the HTTP layer runs anywhere, but VM operations will fail off-target.
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

	"agentbox/internal/config"
	"agentbox/internal/hostagent"
	"agentbox/internal/logging"
)

// Config holds every environment-derived setting the Host Agent needs,
// loaded once in loadConfig rather than scattered across main().
type Config struct {
	ListenAddr          string
	DataDir             string
	FirecrackerBinary   string
	KernelImagePath     string
	GuestPort           int
	BootTimeout         time.Duration
	SubnetBaseCIDR      string
	SubnetPoolSize      int
	SquidConfDir        string // empty disables egress proxying entirely
	SquidPort           int
	JailerEnabled       bool
	JailerBinary        string
	JailerChrootBaseDir string
	JailerUID           int
	JailerGID           int
	LogLevel            string
	LogFormat           string
}

func loadConfig() (Config, error) {
	var cfg Config
	var err error

	cfg.ListenAddr = config.String("LISTEN_ADDR", ":9000")
	cfg.DataDir = config.String("DATA_DIR", "/data")
	cfg.FirecrackerBinary = config.String("FIRECRACKER_BINARY", "/usr/local/bin/firecracker")
	cfg.KernelImagePath = config.String("KERNEL_IMAGE_PATH", "/data/vmlinux")
	cfg.SubnetBaseCIDR = config.String("SUBNET_BASE_CIDR", "172.16.0.0/16")
	// SQUID_CONF_DIR (e.g. "/etc/squid/conf.d") has no default: unset is a
	// deliberate, valid configuration (egress proxying disabled), not a
	// missing value, so it's read as a plain optional string.
	cfg.SquidConfDir = config.String("SQUID_CONF_DIR", "")
	cfg.JailerBinary = config.String("JAILER_BINARY", "/usr/local/bin/jailer")
	cfg.JailerChrootBaseDir = config.String("JAILER_CHROOT_BASE_DIR", "/srv/jailer")
	cfg.LogLevel = config.String("LOG_LEVEL", "info")
	cfg.LogFormat = config.String("LOG_FORMAT", "text")

	if cfg.GuestPort, err = config.Int("GUEST_PORT", 8080); err != nil {
		return cfg, fmt.Errorf("load config: %w", err)
	}
	if cfg.BootTimeout, err = config.Duration("BOOT_TIMEOUT", 10*time.Second); err != nil {
		return cfg, fmt.Errorf("load config: %w", err)
	}
	if cfg.SubnetPoolSize, err = config.Int("SUBNET_POOL_SIZE", 1024); err != nil {
		return cfg, fmt.Errorf("load config: %w", err)
	}
	if cfg.SquidPort, err = config.Int("SQUID_PORT", hostagent.DefaultSquidPort); err != nil {
		return cfg, fmt.Errorf("load config: %w", err)
	}
	if cfg.JailerEnabled, err = config.Bool("JAILER_ENABLED", false); err != nil {
		return cfg, fmt.Errorf("load config: %w", err)
	}
	if cfg.JailerUID, err = config.Int("JAILER_UID", 0); err != nil {
		return cfg, fmt.Errorf("load config: %w", err)
	}
	if cfg.JailerGID, err = config.Int("JAILER_GID", 0); err != nil {
		return cfg, fmt.Errorf("load config: %w", err)
	}
	return cfg, nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "hostagent: "+err.Error())
		os.Exit(1)
	}
	logger := logging.New(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)

	ops, err := hostagent.NewLinuxHostOps(cfg.DataDir, cfg.FirecrackerBinary, 0, cfg.SubnetBaseCIDR, cfg.SubnetPoolSize)
	if err != nil {
		logger.Error("failed to initialize host ops", "data_dir", cfg.DataDir, "subnet_base_cidr", cfg.SubnetBaseCIDR, "error", err)
		os.Exit(1)
	}
	if cfg.SquidConfDir != "" {
		ops.Squid = &hostagent.SquidManager{ConfDir: cfg.SquidConfDir}
		ops.SquidPort = cfg.SquidPort
	} else {
		logger.Warn("SQUID_CONF_DIR not set — egress proxying disabled; instances will have no outbound network access at all (§4.8: iptables locks TAP traffic to the squid port regardless, and with no Squid ACLs applied nothing reaches it)")
	}
	if cfg.JailerEnabled {
		if cfg.JailerUID == 0 || cfg.JailerGID == 0 {
			logger.Warn("JAILER_ENABLED=true but JAILER_UID/JAILER_GID are unset (defaulting to 0/root) — the whole point of the Jailer is dropping root privilege, running it as root defeats that (§4.6/Phase 6)")
		}
		ops.Jailer = &hostagent.JailerConfig{
			Enabled: true, JailerBinary: cfg.JailerBinary, ChrootBaseDir: cfg.JailerChrootBaseDir,
			UID: cfg.JailerUID, GID: cfg.JailerGID,
		}
		logger.Info("jailer enabled (unverified against a real jailer binary — see JailerConfig's doc comment)",
			"jailer_binary", cfg.JailerBinary, "chroot_base_dir", cfg.JailerChrootBaseDir, "uid", cfg.JailerUID, "gid", cfg.JailerGID)
	}
	mgr := hostagent.NewVMManager(
		ops,
		func(socketPath string) hostagent.FirecrackerClient {
			return hostagent.NewUnixSocketFirecrackerClient(socketPath)
		},
		&hostagent.TCPReadinessChecker{},
		hostagent.Config{
			KernelImagePath: cfg.KernelImagePath,
			GuestPort:       cfg.GuestPort,
			BootTimeout:     cfg.BootTimeout,
		},
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: logging.HTTPMiddleware(logger, hostagent.NewRouter(mgr))}
	go func() {
		logger.Info("host agent listening", "addr", cfg.ListenAddr, "data_dir", cfg.DataDir,
			"firecracker_binary", cfg.FirecrackerBinary, "kernel_image_path", cfg.KernelImagePath, "guest_port", cfg.GuestPort)
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
