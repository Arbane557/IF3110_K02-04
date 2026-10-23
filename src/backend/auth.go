// have 1 db
// auth
// manages session

package main

import (
    "context"
    "net/http"
    "time"	
	"database/sql"
)

type User struct {
	ID int64 `json:"id"`
	Username string `json:"username"`
	IsAdmin bool `json:"IsAdmin"`
}

type LoginRequest struct {
	Username string `json:"username"`
 	Password string `json:"password"`
}

func (app *App) requireAuth(next http.Handler) http.Handler
func (app *App) login(w http.ResponseWriter, r *http.Request)
func (app *App) logout(w http.ResponseWriter, r *http.Request)
func (app *App) whoami(w http.ResponseWriter, r *http.Request)

func (app *App) findUserByUsername (c context.Context, username string) (User, string, error)
func (app *App) createNewSession (c context.Context, User user) (token string, expires_at time.Time, error)
func (app *App) checkUserBySession(c context.Context, token string) (User, error)
func (app *App) revokeSession (c context.Context, token string) error
func setSessionCookie(token string) 
func clearSessionCookie(w http.ResponseWriter)