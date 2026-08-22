// Command controller runs the Controller service (design doc §4.2): the
// generic compute-provisioning layer that owns Workload registration and
// the Instance lifecycle state machine. It is the sole owner of its Redis
// data store — the REST API Service never connects to it directly (§4.4).
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"containerised-agents/internal/common"
	"containerised-agents/internal/controller"
)

func main() {
	redisAddr := envOr("REDIS_ADDR", "localhost:6379")
	listenAddr := envOr("LISTEN_ADDR", ":9090")
	tokenSecret := os.Getenv("ROUTING_TOKEN_SECRET")
	if tokenSecret == "" {
		log.Fatal("ROUTING_TOKEN_SECRET must be set (shared secret between Controller and REST API Service, §4.2 routing token contract)")
	}
	idleReaperInterval := envDurationOr("IDLE_REAPER_INTERVAL", 30*time.Second)

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("cannot reach redis at %s: %v", redisAddr, err)
	}

	store := controller.NewStore(rdb)
	tokens := controller.NewTokenIssuer([]byte(tokenSecret))
	ha := controller.NewHTTPHostAgentClient()

	// Image Builder (Phase 2 of the implementation plan) isn't built yet —
	// this stub lets the Controller run end-to-end for everything except
	// actually producing a bootable rootfs. Every workload registered
	// against this binary will sit in PROVISIONING until Phase 2 lands.
	ib := &notImplementedImageBuilder{}

	svc := controller.NewService(store, ha, tokens, ib)

	if err := seedHostsFromEnv(ctx, store); err != nil {
		log.Fatalf("seed host registry: %v", err)
	}

	reaper := controller.NewIdleReaper(svc, idleReaperInterval)
	go reaper.Run(ctx)

	srv := &http.Server{Addr: listenAddr, Handler: controller.NewRouter(svc)}
	go func() {
		log.Printf("controller listening on %s (redis=%s, idle-reaper every %s)", listenAddr, redisAddr, idleReaperInterval)
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

// notImplementedImageBuilder is a placeholder for Phase 2 (Docker image →
// rootfs pipeline, §4.6), not yet implemented.
type notImplementedImageBuilder struct{}

func (notImplementedImageBuilder) Build(ctx context.Context, workloadID, imageRef string) (string, error) {
	return "", errWorkloadBuildNotImplemented
}

var errWorkloadBuildNotImplemented = &notImplementedError{"image builder not implemented yet (Phase 2)"}

type notImplementedError struct{ msg string }

func (e *notImplementedError) Error() string { return e.msg }

// seedHostsFromEnv registers the static Host Agent list (§4.2: "seeded from
// static config at startup, no dynamic host auto-registration in v1").
// HOST_AGENTS format: "host-1=10.0.1.5:9000,host-2=10.0.1.6:9000"
func seedHostsFromEnv(ctx context.Context, store *controller.Store) error {
	raw := os.Getenv("HOST_AGENTS")
	if raw == "" {
		log.Print("HOST_AGENTS not set — starting with no registered hosts; CreateInstance will fail until hosts are registered")
		return nil
	}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			log.Printf("skipping malformed HOST_AGENTS entry: %q", pair)
			continue
		}
		hostID, addr := parts[0], parts[1]
		if err := store.UpsertHost(ctx, &common.Host{
			HostID:        hostID,
			InternalAddr:  addr,
			Status:        common.HostHealthy,
			LastHeartbeat: time.Now().Unix(),
		}); err != nil {
			return err
		}
		log.Printf("registered host %s at %s", hostID, addr)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
