package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/dsmithson/EngineeringActivity/internal/store"
	"github.com/dsmithson/EngineeringActivity/internal/web"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // zone rules for range alignment without relying on the host OS
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func main() {
	if err := run(); err != nil {
		slog.New(slog.NewJSONHandler(os.Stdout, nil)).Error(err.Error(), "service", "web")
		os.Exit(1)
	}
}
func run() error {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if command == "healthcheck" {
		return healthcheck(env("LISTEN_ADDR", "127.0.0.1:8080"))
	}
	if command == "migrate" {
		return fmt.Errorf("database migrations are managed by Liquibase; use docker compose run --rm liquibase")
	}
	if command == "collect" {
		return fmt.Errorf("collection moved to the separate collector executable: collector --mode scheduled")
	}
	if command != "serve" {
		return fmt.Errorf("usage: dashboard [serve|healthcheck]")
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "web")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	demo := os.Getenv("DEMO_MODE") == "true"
	var db *store.Store
	if !demo {
		url := os.Getenv("DATABASE_URL")
		if url == "" {
			return fmt.Errorf("DATABASE_URL is required (or DEMO_MODE=true for an in-memory sample preview)")
		}
		var e error
		db, e = store.Open(ctx, url)
		if e != nil {
			return fmt.Errorf("connect to PostgreSQL: %w", e)
		}
		defer db.Close()
	}
	user, password := os.Getenv("DASHBOARD_USERNAME"), os.Getenv("DASHBOARD_PASSWORD")
	allowAnonymous := os.Getenv("ALLOW_UNAUTHENTICATED") == "true" || os.Getenv("ALLOW_UNAUTHENTICATED_DEMO") == "true"
	if !demo && (user == "" || password == "") && !allowAnonymous {
		return fmt.Errorf("live mode requires DASHBOARD_USERNAME and DASHBOARD_PASSWORD (or ALLOW_UNAUTHENTICATED=true behind trusted ingress/loopback); terminate HTTPS at ingress")
	}
	if (user == "") != (password == "") {
		return fmt.Errorf("set both DASHBOARD_USERNAME and DASHBOARD_PASSWORD")
	}
	addr := env("LISTEN_ADDR", "127.0.0.1:8080")
	if err := validateDemoBind(addr, demo, user, allowAnonymous); err != nil {
		return err
	}
	app := &web.Server{Demo: demo, Username: user, Password: password, Log: log, Version: version, Branding: web.Branding{
		Organization: os.Getenv("BRAND_ORGANIZATION"),
		Team:         os.Getenv("BRAND_TEAM"),
		Scope:        os.Getenv("BRAND_SCOPE"),
		LinkLabel:    os.Getenv("BRAND_LINK_LABEL"),
		LinkURL:      os.Getenv("BRAND_LINK_URL"),
	}}
	if db != nil {
		app.DB = db
	}
	server := &http.Server{Addr: addr, Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	// Metrics are operational aggregates only and are served on a separate listener that
	// is not exposed through ingress, so Prometheus can scrape without dashboard login.
	if metricsAddr := os.Getenv("METRICS_ADDR"); metricsAddr != "" {
		metrics := &http.Server{Addr: metricsAddr, Handler: metricsMux(app), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second}
		go func() {
			if err := serve(ctx, metrics); err != nil {
				log.Error("metrics listener stopped", "error", err.Error())
			}
		}()
		log.Info("metrics listening", "addr", metricsAddr)
	}
	log.Info("dashboard listening", "addr", addr, "demo", demo, "version", version)
	return serve(ctx, server)
}

func metricsMux(app *web.Server) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", app.MetricsHandler())
	return mux
}

func validateDemoBind(addr string, demo bool, user string, allowPublic bool) error {
	if !demo || user != "" || allowPublic {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	if err == nil && (host == "localhost" || (ip != nil && ip.IsLoopback())) {
		return nil
	}
	return fmt.Errorf("unauthenticated demo requires a loopback LISTEN_ADDR; for containers explicitly set ALLOW_UNAUTHENTICATED_DEMO=true and publish the port on host loopback")
}

// Wait for active requests to drain before main exits on SIGTERM.
func serve(ctx context.Context, server *http.Server) error {
	done := make(chan struct{})
	shutdownResult := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := server.Shutdown(shutdown)
			if err != nil {
				_ = server.Close()
			}
			shutdownResult <- err
		case <-done:
		}
	}()
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
		return <-shutdownResult
	}
	close(done)
	return err
}

func healthcheck(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid LISTEN_ADDR: %w", err)
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	} else if host == "::" {
		host = "::1"
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	response, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return fmt.Errorf("healthcheck failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck returned HTTP %d", response.StatusCode)
	}
	return nil
}
