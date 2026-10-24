package main

import (
	"context"
	"database/sql"
	"html/template"
	"log"
	"net/http"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"wbd/backend/auth"
	"wbd/backend/database"
	"wbd/backend/migration"
)

type App struct {
	Auth         *auth.Handler
	DatabaseAPI  *database.Handler
	DatabasePage *database.Viewer
	Migration     *migration.Handler
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

	app := &App{
		Auth:          auth.NewHandler(auth.NewService(internalDB), auth.NewViewer(templates)),
		DatabasePage:  database.NewViewer(templates),
		Migration:     migration.NewHandler(migration.NewViewer(templates)),
	}
	server := &http.Server{
		Addr:              ":6969",
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Println("server running on http://localhost:6969")
	log.Fatal(server.ListenAndServe())
}
