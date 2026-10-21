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
	DB *sql.db
}

func main() {
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	app := &App{DB:db}
}