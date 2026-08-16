package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	controlapi "github.com/bian-cloud-skill/agentos/internal/control/api"
	"github.com/bian-cloud-skill/agentos/internal/control/auth"
	postgresstore "github.com/bian-cloud-skill/agentos/internal/kernel/store/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	databaseURL := flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection URL (or DATABASE_URL)")
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	devTenant := flag.String("dev-tenant", "", "fixed development tenant; requires a loopback listen address")
	flag.Parse()

	if *databaseURL == "" || strings.TrimSpace(*devTenant) == "" {
		slog.Error("database URL and -dev-tenant are required")
		os.Exit(2)
	}
	if !loopbackAddress(*listen) {
		slog.Error("static development identity may only listen on loopback", "listen", *listen)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(ctx, *databaseURL)
	if err != nil {
		slog.Error("create PostgreSQL pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		slog.Error("connect to PostgreSQL", "error", err)
		os.Exit(1)
	}

	repository := postgresstore.New(pool)
	handler := controlapi.NewHandler(repository, repository, repository)
	handler = auth.StaticMiddleware(auth.Principal{Subject: "local-developer", TenantID: *devTenant}, handler)
	server := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	slog.Info("Agent OS development control API listening", "address", *listen, "tenant", *devTenant)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("serve control API", "error", err)
		os.Exit(1)
	}
}

func loopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
