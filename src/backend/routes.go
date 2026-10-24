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
	mux.HandleFunc("GET /login", app.Auth.AuthPage)
	mux.HandleFunc("POST /login", app.Auth.Login)
	mux.HandleFunc("POST /logout", app.Auth.Logout)
	mux.HandleFunc("GET /whoami", app.Auth.Whoami)
	mux.Handle("GET /auth", app.Auth.RequireAuthAdmin(http.HandlerFunc(app.Auth.UsersPage)))
	mux.Handle("GET /groups", app.Auth.RequireAuthAdmin(http.HandlerFunc(app.Auth.GroupsPage)))
	mux.Handle("GET /groups/{groupID}", app.Auth.RequireAuthAdmin(http.HandlerFunc(app.Auth.GroupPage)))
	mux.Handle("POST /api/auth/register", app.Auth.RequireAuthAdmin(http.HandlerFunc(app.Auth.RegisterUser)))
	mux.Handle("GET /api/auth/groups", app.Auth.RequireAuthAdmin(http.HandlerFunc(app.Auth.ListGroups)))
	mux.Handle("POST /api/auth/groups", app.Auth.RequireAuthAdmin(http.HandlerFunc(app.Auth.CreateGroup)))
	mux.Handle("GET /api/auth/groups/{groupID}", app.Auth.RequireAuthAdmin(http.HandlerFunc(app.Auth.GetGroup)))
	mux.Handle("POST /api/auth/groups/{groupID}/members", app.Auth.RequireAuthAdmin(http.HandlerFunc(app.Auth.AddGroupMember)))
	mux.Handle("DELETE /api/auth/groups/{groupID}/members/{userID}", app.Auth.RequireAuthAdmin(http.HandlerFunc(app.Auth.RemoveGroupMember)))
	mux.Handle("PUT /api/auth/groups/{groupID}/databases/{databaseID}", app.Auth.RequireAuthAdmin(http.HandlerFunc(app.Auth.SetGroupDatabaseAccess)))
}

func (app *App) registerDatabaseRoutes(mux *http.ServeMux) {
	db := app.Database
	mux.HandleFunc("GET /api/databases", db.ListDatabases)
	mux.HandleFunc("POST /api/databases", db.CreateDatabase)
	mux.HandleFunc("GET /api/databases/{databaseID}", db.GetDatabase)
	mux.HandleFunc("PUT /api/databases/{databaseID}", db.UpdateDatabase)
	mux.HandleFunc("DELETE /api/databases/{databaseID}", db.DeleteDatabase)
	mux.HandleFunc("POST /api/databases/{databaseID}/test-connection", db.TestConnection)
	mux.HandleFunc("GET /api/databases/{databaseID}/schemas", db.ListSchemas)
	mux.HandleFunc("GET /api/databases/{databaseID}/schemas/{schema}/tables", db.ListTables)
	mux.HandleFunc("GET /api/databases/{databaseID}/schemas/{schema}/tables/{table}", db.GetTableDetails)
	mux.HandleFunc("GET /api/databases/{databaseID}/schemas/{schema}/tables/{table}/rows", db.GetRows)
}

func (app *App) registerMigrationRoutes(mux *http.ServeMux) {
	migration := app.Migration
	mux.HandleFunc("GET /api/databases/{databaseID}/migrations", migration.ListMigrations)
	mux.HandleFunc("POST /api/databases/{databaseID}/migrations", migration.CreateMigration)
	mux.HandleFunc("GET /api/databases/{databaseID}/migrations/{version}", migration.GetMigration)
	mux.HandleFunc("PUT /api/databases/{databaseID}/migrations/{version}", migration.UpdateMigration)
	mux.HandleFunc("POST /api/databases/{databaseID}/migrations/{version}/execute", migration.ExecuteMigration)
}

