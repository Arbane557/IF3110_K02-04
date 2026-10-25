package main

import "net/http"

func (app *App) routes() http.Handler {
	mux := http.NewServeMux()
	app.registerPageRoutes(mux)
	app.registerAuthRoutes(mux)
	app.registerDatabaseRoutes(mux)
	app.registerMigrationRoutes(mux)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	return mux
}

func (app *App) registerPageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /health", app.healthHandler)
	mux.Handle("GET /databases", app.Auth.RequireAuth(http.HandlerFunc(app.Database.DatabasePage)))
	mux.Handle("GET /migrations", app.Auth.RequireAuth(http.HandlerFunc(app.Migration.MigrationPage)))
}

func (app *App) registerAuthRoutes(mux *http.ServeMux) {
	csrf := func(handler http.HandlerFunc) http.Handler { return app.Auth.RequireCSRF(handler) }
	admin := func(handler http.HandlerFunc) http.Handler {
		return app.Auth.RequireAuthAdmin(csrf(handler))
	}

	mux.HandleFunc("GET /login", app.Auth.AuthPage)
	mux.Handle("POST /login", csrf(app.Auth.Login))
	mux.Handle("POST /logout", csrf(app.Auth.Logout))
	mux.HandleFunc("GET /whoami", app.Auth.Whoami)
	mux.Handle("GET /auth", app.Auth.RequireAuthAdmin(http.HandlerFunc(app.Auth.UsersPage)))
	mux.Handle("GET /groups", app.Auth.RequireAuthAdmin(http.HandlerFunc(app.Auth.GroupsPage)))
	mux.Handle("GET /groups/{groupID}", app.Auth.RequireAuthAdmin(http.HandlerFunc(app.Auth.GroupPage)))
	mux.Handle("POST /api/auth/register", admin(app.Auth.RegisterUser))
	mux.Handle("GET /api/auth/groups", app.Auth.RequireAuthAdmin(http.HandlerFunc(app.Auth.ListGroups)))
	mux.Handle("POST /api/auth/groups", admin(app.Auth.CreateGroup))
	mux.Handle("GET /api/auth/groups/{groupID}", app.Auth.RequireAuthAdmin(http.HandlerFunc(app.Auth.GetGroup)))
	mux.Handle("POST /api/auth/groups/{groupID}/members", admin(app.Auth.AddGroupMember))
	mux.Handle("DELETE /api/auth/groups/{groupID}/members/{userID}", admin(app.Auth.RemoveGroupMember))
	mux.Handle("PUT /api/auth/groups/{groupID}/databases/{databaseID}", admin(app.Auth.SetGroupDatabaseAccess))
}

func (app *App) registerDatabaseRoutes(mux *http.ServeMux) {
	db := app.Database
	authenticated := func(handler http.HandlerFunc) http.Handler {
		return app.Auth.RequireAuthAPI(handler)
	}
	admin := func(handler http.HandlerFunc) http.Handler {
		return app.Auth.RequireAuthAdmin(app.Auth.RequireCSRF(handler))
	}

	mux.Handle("GET /api/databases", authenticated(db.ListDatabases))
	mux.Handle("POST /api/databases", admin(db.CreateDatabase))
	mux.Handle("GET /api/databases/{databaseID}", authenticated(db.GetDatabase))
	mux.Handle("PUT /api/databases/{databaseID}", admin(db.UpdateDatabase))
	mux.Handle("DELETE /api/databases/{databaseID}", admin(db.DeleteDatabase))
	mux.Handle("POST /api/databases/{databaseID}/test-connection", admin(db.TestConnection))
	mux.Handle("GET /api/databases/{databaseID}/schemas", authenticated(db.ListSchemas))
	mux.Handle("GET /api/databases/{databaseID}/schemas/{schema}/tables", authenticated(db.ListTables))
	mux.Handle("GET /api/databases/{databaseID}/schemas/{schema}/tables/{table}", authenticated(db.GetTableDetails))
	mux.Handle("GET /api/databases/{databaseID}/schemas/{schema}/tables/{table}/rows", authenticated(db.GetRows))
}

func (app *App) registerMigrationRoutes(mux *http.ServeMux) {
	migration := app.Migration
	get := func(handler http.HandlerFunc) http.Handler { return app.Auth.RequireAuthAPI(handler) }
	change := func(handler http.HandlerFunc) http.Handler {
		return app.Auth.RequireAuthAPI(app.Auth.RequireCSRF(handler))
	}

	mux.Handle("GET /api/databases/{databaseID}/migrations", get(migration.ListMigrations))
	mux.Handle("POST /api/databases/{databaseID}/migrations", change(migration.CreateMigration))
	mux.Handle("GET /api/databases/{databaseID}/migrations/{version}", get(migration.GetMigration))
	mux.Handle("PUT /api/databases/{databaseID}/migrations/{version}", change(migration.UpdateMigration))
	mux.Handle("POST /api/databases/{databaseID}/migrations/{version}/execute", change(migration.ExecuteMigration))
}
