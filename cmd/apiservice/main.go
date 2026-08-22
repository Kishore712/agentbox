// Command apiservice runs the REST API Service (design doc §4.1): the only
// public-facing surface. Stateless — no Redis dependency, no direct access
// to the Controller's data. Every read and every invocation goes through
// the Controller's API.
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

	"containerised-agents/internal/apiservice"
)

func main() {
	listenAddr := envOr("LISTEN_ADDR", ":8080")
	controllerURL := envOr("CONTROLLER_URL", "http://localhost:9090")
	apiKey := os.Getenv("API_KEY")
	if apiKey == "" {
		log.Fatal("API_KEY must be set (§8: single static key for the prototype)")
	}
	tokenSecret := os.Getenv("ROUTING_TOKEN_SECRET")
	if tokenSecret == "" {
		log.Fatal("ROUTING_TOKEN_SECRET must be set (must match the Controller's ROUTING_TOKEN_SECRET, §4.2 routing token contract)")
	}
	guestProxyTimeout := envDurationOr("GUEST_PROXY_TIMEOUT", 30*time.Second)

	ctrl := apiservice.NewHTTPControllerClient(controllerURL)
	tokens := apiservice.NewTokenVerifier([]byte(tokenSecret))
	proxy := apiservice.NewHTTPGuestProxy(guestProxyTimeout)
	svc := apiservice.NewService(ctrl, tokens, proxy)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: listenAddr, Handler: apiservice.NewRouter(svc, apiKey)}
	go func() {
		log.Printf("api service listening on %s (controller=%s)", listenAddr, controllerURL)
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
