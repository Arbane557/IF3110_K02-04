package database

import (
	"bytes"
	"context"
	"encoding/json"
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
	if record.Config.Password != "" {
		t.Fatal("plaintext credential reached repository config")
	}
	original := append([]byte(nil), record.PasswordCiphertext...)

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
	if !bytes.Equal(original, record.PasswordCiphertext) {
		t.Fatal("blank password unnecessarily replaced stored ciphertext")
	}
	updated.Password = "replacement-secret"
	if _, err := service.Update(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	record, err = repository.FindByID(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	password, err = protector.Open(record.PasswordCiphertext)
	if err != nil || password != updated.Password || record.Config.Password != "" {
		t.Fatal("password replacement was not encrypted correctly")
	}
}

func testProtector(t *testing.T) CredentialProtector {
	t.Helper()
	protector, err := NewAESGCMProtector([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("new protector: %v", err)
	}
	return protector
}

func TestCredentialProtectorRejectsTamperedCiphertext(t *testing.T) {
	protector := testProtector(t)
	ciphertext, err := protector.Seal("secret")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err := protector.Open(ciphertext); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
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
	if u.Hostname() != "2001:db8::1" || u.Port() != "5432" || u.User.Username() != "user@name" || password != "p@ss word" || u.Path != "/my db" || u.Query().Get("sslmode") != "verify-full" {
		t.Fatal("DSN did not preserve host, database, credentials, or SSL mode")
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
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["password"]; ok {
		t.Fatal("password field present in response")
	}
	if body["name"] != "inventory" || body["id"] != float64(1) {
		t.Fatalf("created response=%+v", body)
	}
}

func TestCredentialProtectorRoundTripAndMalformedInput(t *testing.T) {
	protector := testProtector(t)
	first, err := protector.Seal("secret")
	if err != nil {
		t.Fatal(err)
	}
	second, err := protector.Seal("secret")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("encrypting twice reused the same nonce/ciphertext")
	}
	for _, ciphertext := range [][]byte{first, second} {
		plaintext, err := protector.Open(ciphertext)
		if err != nil || plaintext != "secret" {
			t.Fatal("encryption round trip failed")
		}
	}
	for _, ciphertext := range [][]byte{nil, []byte("short"), first[:12]} {
		if _, err := protector.Open(ciphertext); err == nil {
			t.Fatal("malformed ciphertext accepted")
		}
	}
	other, err := NewAESGCMProtector([]byte("abcdef0123456789abcdef0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(first); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestPaginationValidationAndNormalization(t *testing.T) {
	for _, tc := range []struct {
		query   string
		want    Page
		invalid bool
	}{
		{"", Page{1, 50}, false}, {"?page=2&pageSize=10", Page{2, 10}, false},
		{"?page=0", Page{}, true}, {"?page=-1", Page{}, true}, {"?page=x", Page{}, true}, {"?pageSize=0", Page{}, true}, {"?pageSize=x", Page{}, true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			got, err := pageFromRequest(httptest.NewRequest("GET", "/rows"+tc.query, nil))
			if (err != nil) != tc.invalid || (!tc.invalid && got != tc.want) {
				t.Fatalf("page=%+v error=%v", got, err)
			}
		})
	}
	for _, tc := range []struct{ input, want Page }{{Page{}, Page{1, 50}}, {Page{2, 1000}, Page{2, 100}}, {Page{2, 10}, Page{2, 10}}} {
		if got := normalizePage(tc.input); got != tc.want {
			t.Fatalf("normalized=%+v want=%+v", got, tc.want)
		}
	}
}

func TestListDatabasesFiltersPermissionLevels(t *testing.T) {
	repository := newMemoryRegistryRepository()
	for i, name := range []string{"hidden", "viewer", "editor"} {
		id := DatabaseID(i + 1)
		repository.records[id] = databaseRecord{Config: DatabaseConfig{ID: id, Name: name}}
	}
	registry := NewRegistryService(repository, nil, nil)
	authorizer := &databaseAccessStub{}
	handler := NewHandler(registry, nil, authorizer, nil)
	response := httptest.NewRecorder()
	handler.ListDatabases(response, httptest.NewRequest("GET", "/api/databases", nil))
	if response.Code != 200 {
		t.Fatalf("status=%d", response.Code)
	}
	var items []DatabaseSummary
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	levels := map[DatabaseID]string{}
	for _, item := range items {
		levels[item.ID] = item.AccessLevel
	}
	if len(items) != 2 || levels[2] != "view" || levels[3] != "edit" {
		t.Fatalf("visible databases=%+v", items)
	}
}

type databaseAccessStub struct{}

func (*databaseAccessStub) PrincipalFromRequest(*http.Request) (auth.Principal, error) {
	return auth.Principal{UserID: 7}, nil
}
func (*databaseAccessStub) AuthorizeDatabase(_ context.Context, _ auth.Principal, id int64, action string) error {
	if id == 3 || (id == 2 && action == auth.ActionView) {
		return nil
	}
	return auth.ErrForbidden
}
