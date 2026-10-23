// hold api routing for other backend

package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type App struct {
	InternalDB *sql.db
}

func main() {
	if os.Getenv("DATABASE_URL") == "" {
		log.Fatal("DATABASE_URL is required")
	}
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))

	defer internalDB.Close()

	internalDB.SetMaxOpenConns(20)
	internalDB.SetMaxIdleConns(5)
	internalDB.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	app := &App{
		InternalDB: internalDB
	}

	server := &http.Server {
		Address ":6969",
		Handler app.routes(),
	}
}