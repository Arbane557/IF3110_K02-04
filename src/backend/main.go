package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"wbd/backend/auth"
	"wbd/backend/database"
	"wbd/backend/migration"
	"wbd/backend/observability"
)

type App struct {
	Auth      *auth.Handler
	Database  *database.Handler
	Migration *migration.Handler
	Metrics   http.Handler
}

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	internalDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		log.Fatal("open database:", err)
	}
	defer internalDB.Close()

	internalDB.SetMaxOpenConns(20)
	internalDB.SetMaxIdleConns(5)
	internalDB.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := internalDB.PingContext(ctx); err != nil {
		log.Fatal("connect database:", err)
	}

	templates, err := template.ParseGlob("templates/*.html")
	if err != nil {
		log.Fatal("parse templates:", err)
	}
	key, err := hex.DecodeString(os.Getenv("CREDENTIAL_ENCRYPTION_KEY"))
	if err != nil || len(key) != 32 {
		log.Fatal("CREDENTIAL_ENCRYPTION_KEY must be 64 hexadecimal characters")
	}
	protector, err := database.NewAESGCMProtector(key)
	if err != nil {
		log.Fatal("create credential protector:", err)
	}
	registryRepository := database.NewPostgresRegistryRepository(internalDB)
	connectionManager := database.NewConnectionManager(registryRepository, protector)
	metricsRegistry := prometheus.NewRegistry()
	metricsRegistry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	tracerContext, cancelTracerContext := context.WithTimeout(context.Background(), 10*time.Second)
	tracerProvider, err := observability.NewTracerProvider(tracerContext)
	cancelTracerContext()
	if err != nil {
		log.Fatal("initialize tracing:", err)
	}
	observabilityService := observability.NewService(
		metricsRegistry,
		tracerProvider.Tracer("wbd/backend"),
	)
	access := auth.NewAccessService(internalDB)
	registry := database.NewRegistryService(registryRepository, connectionManager, protector)
	catalog := database.NewCatalogService(connectionManager)
	migrationService := migration.NewService(internalDB, connectionManager, observabilityService)

	app := &App{
		Auth:      auth.NewHandler(auth.NewService(internalDB, protector), templates),
		Database:  database.NewHandler(registry, catalog, access, templates),
		Migration: migration.NewHandler(templates, migrationService, access),
		Metrics:   promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{}),
	}

	defer connectionManager.Close()

	server := &http.Server{
		Addr: ":6969",
		Handler: otelhttp.NewHandler(app.routes(), observability.SpanHTTP,
			otelhttp.WithFilter(func(r *http.Request) bool {
				return r.URL.Path != "/metrics" && r.URL.Path != "/health"
			}),
		),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Println("server running on http://localhost:6969")
	server.ListenAndServe()
}

func (app *App) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
