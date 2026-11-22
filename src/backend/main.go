package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"wbd/backend/auth"
	"wbd/backend/database"
	"wbd/backend/db"
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

	upgradeCtx, cancelUpgrade := context.WithTimeout(context.Background(), 30*time.Second)
	err = db.InstallMigrationJobs(upgradeCtx, internalDB)
	cancelUpgrade()
	if err != nil {
		log.Fatal("install migration job schema:", err)
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
	metricsRegistry := prometheus.NewRegistry()
	metricsRegistry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewDBStatsCollector(internalDB, "platform"),
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
	connectionManager := database.NewConnectionManager(registryRepository, protector, observabilityService)
	metricsHandler := observabilityService.MetricsHandler(metricsRegistry, connectionManager.PoolStats)
	access := auth.NewAccessService(internalDB)
	registry := database.NewRegistryService(registryRepository, connectionManager, protector)
	catalog := database.NewCatalogService(connectionManager)
	migrationService := migration.NewService(internalDB, connectionManager, observabilityService)

	defer connectionManager.Close()
	if len(os.Args) == 2 && os.Args[1] == "worker" {
		brokerURL := os.Getenv("RABBITMQ_URL")
		if brokerURL == "" {
			log.Fatal("RABBITMQ_URL is required in worker mode")
		}
		workerCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		listener, err := net.Listen("tcp", ":9091")
		if err != nil {
			log.Fatal("listen worker metrics:", err)
		}
		metricsServer := &http.Server{
			Handler: workerMetricsRoutes(metricsHandler),
			ReadHeaderTimeout: 5 * time.Second,
			WriteTimeout: 10 * time.Second,
		}
		go func() {
			if err := metricsServer.Serve(listener); err != nil && err != http.ErrServerClosed {
				log.Print("worker metrics server stopped: ", err)
				stop()
			}
		}()
		migrationService.RunWorker(workerCtx, brokerURL, 4)
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		_ = metricsServer.Shutdown(shutdownCtx)
		_ = tracerProvider.Shutdown(shutdownCtx)
		return
	}
	metricsRegistry.MustRegister(migration.NewJobCollector(internalDB))

	templates, err := template.ParseGlob("templates/*.html")
	if err != nil {
		log.Fatal("parse templates:", err)
	}
	app := &App{
		Auth:      auth.NewHandler(auth.NewService(internalDB, protector), templates),
		Database:  database.NewHandler(registry, catalog, access, templates),
		Migration: migration.NewHandler(templates, migrationService, access),
		Metrics:   metricsHandler,
	}

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
