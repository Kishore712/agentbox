// Command hostagent runs the Host Agent service (design doc §4.3): the
// systemd service on each KVM-enabled GCE host that actually touches
// Firecracker. Only functional on Linux with KVM, the `firecracker`
// binary, and root privileges for TAP/iptables — everything here compiles
// and the HTTP layer runs anywhere, but VM operations will fail off-target.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"containerised-agents/internal/hostagent"
)

func main() {
	listenAddr := envOr("LISTEN_ADDR", ":9000")
	dataDir := envOr("DATA_DIR", "/data")
	firecrackerBinary := envOr("FIRECRACKER_BINARY", "/usr/local/bin/firecracker")
	kernelImagePath := envOr("KERNEL_IMAGE_PATH", "/data/vmlinux")
	guestPort := envIntOr("GUEST_PORT", 8080)
	bootTimeout := envDurationOr("BOOT_TIMEOUT", 10*time.Second)

	ops := &hostagent.LinuxHostOps{
		DataDir:           dataDir,
		FirecrackerBinary: firecrackerBinary,
	}
	mgr := hostagent.NewVMManager(
		ops,
		func(socketPath string) hostagent.FirecrackerClient {
			return hostagent.NewUnixSocketFirecrackerClient(socketPath)
		},
		&hostagent.TCPReadinessChecker{},
		hostagent.Config{
			KernelImagePath: kernelImagePath,
			GuestPort:       guestPort,
			BootTimeout:     bootTimeout,
		},
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: listenAddr, Handler: hostagent.NewRouter(mgr)}
	go func() {
		log.Printf("host agent listening on %s (data_dir=%s, firecracker=%s, kernel=%s, guest_port=%d)",
			listenAddr, dataDir, firecrackerBinary, kernelImagePath, guestPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("invalid %s=%q, using default %d", key, v, fallback)
		return fallback
	}
	return n
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	secs, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("invalid %s=%q, using default %s", key, v, fallback)
		return fallback
	}
	return time.Duration(secs) * time.Second
}
