package main

import (
	"context"
	"database/sql"
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/joho/godotenv"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	glimpse "github.com/coruben/glimpse"
	gendb "github.com/coruben/glimpse/db"
)

//go:embed ui/index.html
var uiFS embed.FS

func main() {
	// Optional .env in the working directory; real env vars win over it.
	if err := godotenv.Load(); err == nil {
		log.Printf("loaded .env")
	}

	dbPath := envOr("DB_PATH", "./data/glimpse.db")
	port := envOr("PORT", "4318")
	retentionHours := envIntOr("RETENTION_HOURS", 24)

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Fatalf("cannot create data dir: %v", err)
	}

	database, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("cannot open database: %v", err)
	}
	defer database.Close()

	// SQLite tuning: WAL for concurrent reads, NORMAL sync is safe with WAL,
	// busy_timeout avoids "database is locked" under write bursts.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := database.Exec(pragma); err != nil {
			log.Fatalf("%s failed: %v", pragma, err)
		}
	}

	goose.SetBaseFS(glimpse.Migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		log.Fatalf("goose dialect: %v", err)
	}
	if err := goose.Up(database, "sql/migrations"); err != nil {
		log.Fatalf("migrations failed: %v", err)
	}

	queries := gendb.New(database)

	go purgeLoop(database, queries, retentionHours)

	router := http.NewServeMux()

	// OTLP/HTTP ingest — the standard path the OpenTelemetry agents use.
	router.HandleFunc("POST /v1/traces", handleOTLPTraces(database, queries))

	var mssql *sql.DB
	if dsn := os.Getenv("GLIMPSE_MSSQL_DSN"); dsn != "" {
		mssql, err = sql.Open("sqlserver", dsn)
		if err != nil {
			log.Fatalf("bad GLIMPSE_MSSQL_DSN: %v", err)
		}
		plansEnabled = true
	}

	config := huma.DefaultConfig("glimpse", "1.0.0")
	config.Info.Description = "Tiny trace collector with code insights. " +
		"Point an OpenTelemetry agent at POST /v1/traces (OTLP/HTTP), " +
		"then read /api/endpoints, /api/insights, /api/traces."
	api := humago.New(router, config)
	registerAPI(api, queries)
	if mssql != nil {
		registerPlanAPI(api, mssql)
		log.Printf("  MSSQL execution plans enabled")
	}

	router.HandleFunc("GET /api/traces/{traceId}/digest", handleTraceDigest(queries))

	uiContent, _ := fs.Sub(uiFS, "ui")
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, uiContent, "index.html")
	}
	router.Handle("/", http.FileServerFS(uiContent))
	router.HandleFunc("/{$}", serveIndex)
	router.HandleFunc("GET /trace/{id}", serveIndex)

	log.Printf("glimpse listening on :%s", port)
	log.Printf("  database:    %s (retention %dh)", dbPath, retentionHours)
	log.Printf("  OTLP ingest: http://localhost:%s/v1/traces", port)
	log.Printf("  UI:          http://localhost:%s/", port)
	log.Printf("  API docs:    http://localhost:%s/docs", port)
	log.Fatal(http.ListenAndServe(":"+port, router))
}

func purgeLoop(database *sql.DB, queries *gendb.Queries, retentionHours int) {
	ticker := time.NewTicker(10 * time.Minute)
	for range ticker.C {
		cutoff := time.Now().Add(-time.Duration(retentionHours) * time.Hour).UnixNano()
		if err := queries.PurgeOldSpans(context.Background(), cutoff); err != nil {
			log.Printf("purge failed: %v", err)
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
