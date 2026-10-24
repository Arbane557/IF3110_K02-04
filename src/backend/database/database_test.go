package database

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"wbd/backend/auth"
)

type memoryRegistryRepository struct {
	nextID  DatabaseID
	records map[DatabaseID]databaseRecord
}

func newMemoryRegistryRepository() *memoryRegistryRepository {
	return &memoryRegistryRepository{records: make(map[DatabaseID]databaseRecord)}
}

func (r *memoryRegistryRepository) Create(_ context.Context, record databaseRecord) (databaseRecord, error) {
	r.nextID++
	record.Config.ID = r.nextID
	r.records[record.Config.ID] = record
	return record, nil
}

func (r *memoryRegistryRepository) List(_ context.Context) ([]DatabaseSummary, error) {
	result := make([]DatabaseSummary, 0, len(r.records))
	for _, record := range r.records {
		result = append(result, DatabaseSummary{ID: record.Config.ID, Name: record.Config.Name})
	}
	return result, nil
}

func (r *memoryRegistryRepository) FindByID(_ context.Context, id DatabaseID) (databaseRecord, error) {
	record, ok := r.records[id]
	if !ok {
		return databaseRecord{}, ErrNotFound
	}
	return record, nil
}

func (r *memoryRegistryRepository) Update(_ context.Context, record databaseRecord) (databaseRecord, error) {
	if _, ok := r.records[record.Config.ID]; !ok {
		return databaseRecord{}, ErrNotFound
	}
	r.records[record.Config.ID] = record
	return record, nil
}

func (r *memoryRegistryRepository) Delete(_ context.Context, id DatabaseID) error {
	if _, ok := r.records[id]; !ok {
		return ErrNotFound
	}
	delete(r.records, id)
	return nil
}

type testAuthorizer struct {
	principal    auth.Principal
	principalErr error
	authorizeErr error
}

func (a testAuthorizer) PrincipalFromRequest(*http.Request) (auth.Principal, error) {
	return a.principal, a.principalErr
}

func (a testAuthorizer) AuthorizeDatabase(context.Context, auth.Principal, int64, string) error {
	return a.authorizeErr
}

func validConfig() DatabaseConfig {
	return DatabaseConfig{
		Name: "inventory", Host: "db.example.test", Port: 5432,
		DatabaseName: "inventory", Username: "viewer", Password: "secret",
		SSLMode: "require",
	}
}

func TestRegistryCreateStoresCredentialAndUpdateRetainsIt(t *testing.T) {
	repository := newMemoryRegistryRepository()
	protector := testProtector(t)
	manager := NewConnectionManager(repository, protector)
	service := NewRegistryService(repository, manager, protector)

	created, err := service.Create(context.Background(), validConfig())
	if err != nil {
		t.Fatalf("create registry: %v", err)
	}
	if created.Password != "" {
		t.Fatal("password was returned from registry service")
	}
	record, err := repository.FindByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("find record: %v", err)
	}
	if strings.Contains(string(record.PasswordCiphertext), "secret") {
		t.Fatal("credential was stored in plaintext")
	}

	updated := validConfig()
	updated.ID = created.ID
	updated.Name = "inventory-renamed"
	updated.Password = ""
	if _, err := service.Update(context.Background(), updated); err != nil {
		t.Fatalf("update registry: %v", err)
	}
	record, err = repository.FindByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("find updated record: %v", err)
	}
	password, err := protector.Open(record.PasswordCiphertext)
	if err != nil || password != "secret" {
		t.Fatalf("stored credential = %q, %v; want original credential", password, err)
	}
}

func testProtector(t *testing.T) CredentialProtector {
	t.Helper()
	protector, err := NewAESGCMProtector([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil { t.Fatalf("new protector: %v", err) }
	return protector
}

func TestCredentialProtectorRejectsTamperedCiphertext(t *testing.T) {
	protector := testProtector(t)
	ciphertext, err := protector.Seal("secret")
	if err != nil { t.Fatalf("seal: %v", err) }
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err := protector.Open(ciphertext); err == nil { t.Fatal("tampered ciphertext was accepted") }
}

func TestRegistryRejectsInvalidConfiguration(t *testing.T) {
	tests := []DatabaseConfig{
		{},
		{Name: "db", Host: "host", Port: 5432, DatabaseName: "app", Username: "user", Password: "secret", SSLMode: "wrong"},
		{Name: "db", Host: "host", Port: 0, DatabaseName: "app", Username: "user", Password: "secret", SSLMode: "disable"},
	}
	for _, config := range tests {
		if !errors.Is(validateConfig(config, true), ErrInvalidConfig) {
			t.Fatalf("config %+v was accepted", config)
		}
	}
}

func TestTargetDSNPreservesEscapedCredentials(t *testing.T) {
	dsn := targetDSN(DatabaseConfig{Host: "2001:db8::1", Port: 5432, DatabaseName: "my db", Username: "user@name", Password: "p@ss word", SSLMode: "verify-full"})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	password, _ := u.User.Password()
	if u.Hostname() != "2001:db8::1" || u.Port() != "5432" || u.User.Username() != "user@name" || password != "p@ss word" || u.Query().Get("sslmode") != "verify-full" {
		t.Fatalf("unexpected DSN: %s", dsn)
	}
}

func TestCreateDatabaseHandlerRejectsNonAdmin(t *testing.T) {
	handler := NewHandler(nil, nil, testAuthorizer{principal: auth.Principal{UserID: 1}}, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/databases", strings.NewReader(`{"name":"db"}`))
	response := httptest.NewRecorder()
	handler.CreateDatabase(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestCreateDatabaseHandlerDoesNotReturnPassword(t *testing.T) {
	repository := newMemoryRegistryRepository()
	protector := testProtector(t)
	registry := NewRegistryService(repository, NewConnectionManager(repository, protector), protector)
	handler := NewHandler(registry, nil, testAuthorizer{principal: auth.Principal{UserID: 1, IsPlatformAdmin: true}}, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/databases", strings.NewReader(`{"name":"inventory","host":"db","port":5432,"databaseName":"app","username":"viewer","sslMode":"disable","password":"top-secret"}`))
	response := httptest.NewRecorder()
	handler.CreateDatabase(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "top-secret") {
		t.Fatal("password was returned in HTTP response")
	}
}
