// SQL Server's self-signed certificates often have a negative serial number,
// which Go 1.23+ rejects during the TLS handshake.
//
//go:debug x509negativeserial=1
package main

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/joho/godotenv"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	glimpse "github.com/lab900-luis-santos/glimpse"
	gendb "github.com/lab900-luis-santos/glimpse/db"
)

//go:embed ui
var uiFS embed.FS

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	// Optional .env in the working directory; real env vars win over it.
	if err := godotenv.Load(); err == nil {
		log.Printf("loaded .env")
	}

	dbPath := envOr("DB_PATH", "./data/glimpse.db")
	host := envOr("HOST", "127.0.0.1")
	port := envOr("PORT", "4318")
	retentionHours := envIntOr("RETENTION_HOURS", 24)

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Fatalf("cannot create data dir: %v", err)
	}

	database, err := openDB(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close()

	queries := gendb.New(database)

	var mssql *sql.DB
	if dsn := os.Getenv("GLIMPSE_MSSQL_DSN"); dsn != "" {
		mssql, err = sql.Open("sqlserver", dsn)
		if err != nil {
			log.Fatalf("bad GLIMPSE_MSSQL_DSN: %v", err)
		}
		plansEnabled = true
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go purgeLoop(ctx, queries, retentionHours)

	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		log.Fatalf("cannot listen: %v", err)
	}
	// No write timeout: it would cut slow responses such as big traces.
	server := &http.Server{
		Handler:           newRouter(database, queries, mssql),
		ReadHeaderTimeout: 10 * time.Second,
	}

	addr := ln.Addr().String()
	log.Printf("glimpse %s listening on %s", version, addr)
	log.Printf("  database:    %s (retention %dh)", dbPath, retentionHours)
	log.Printf("  OTLP ingest: http://%s/v1/traces", addr)
	log.Printf("  UI:          http://%s/", addr)
	log.Printf("  API docs:    http://%s/docs", addr)
	if mssql != nil {
		log.Printf("  MSSQL execution plans enabled")
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(ln) }()

	select {
	case err := <-serveErr:
		log.Fatalf("server failed: %v", err)
	case <-ctx.Done():
	}
	log.Printf("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// openDB opens the SQLite file and runs the embedded migrations.
func openDB(dbPath string) (*sql.DB, error) {
	// The pragmas go in the DSN because they are per connection and
	// database/sql opens more than one. WAL for concurrent reads, NORMAL sync
	// is safe with WAL, busy_timeout avoids "database is locked" under write bursts.
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("cannot open database: %w", err)
	}

	goose.SetBaseFS(glimpse.Migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		database.Close()
		return nil, fmt.Errorf("goose dialect: %w", err)
	}
	if err := goose.Up(database, "sql/migrations"); err != nil {
		database.Close()
		return nil, fmt.Errorf("migrations failed: %w", err)
	}
	return database, nil
}

// newRouter wires ingest, the API, the digest and the UI.
func newRouter(database *sql.DB, queries *gendb.Queries, mssql *sql.DB) http.Handler {
	router := http.NewServeMux()

	// OTLP/HTTP ingest — the standard path the OpenTelemetry agents use.
	router.HandleFunc("POST /v1/traces", handleOTLPTraces(database, queries))

	config := huma.DefaultConfig("glimpse", version)
	config.Info.Description = "Tiny trace collector with code insights. " +
		"Point an OpenTelemetry agent at POST /v1/traces (OTLP/HTTP), " +
		"then read /api/endpoints, /api/insights, /api/traces."
	api := humago.New(router, config)
	registerAPI(api, database, queries)
	if mssql != nil {
		registerPlanAPI(api, mssql)
	}

	router.HandleFunc("GET /api/traces/{traceId}/digest", handleTraceDigest(queries))

	uiContent, _ := fs.Sub(uiFS, "ui")
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, uiContent, "index.html")
	}
	router.Handle("/", http.FileServerFS(uiContent))
	router.HandleFunc("/{$}", serveIndex)
	router.HandleFunc("GET /trace/{id}", serveIndex)
	return router
}

// purgeLoop purges once at startup, then every 10 minutes until ctx ends.
func purgeLoop(ctx context.Context, queries *gendb.Queries, retentionHours int) {
	purge := func() {
		cutoff := time.Now().Add(-time.Duration(retentionHours) * time.Hour).UnixNano()
		if err := queries.PurgeOldSpans(ctx, cutoff); err != nil && ctx.Err() == nil {
			log.Printf("purge failed: %v", err)
		}
	}
	purge()
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			purge()
		case <-ctx.Done():
			return
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}
