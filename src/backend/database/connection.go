package database

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TargetPoolProvider is the connection contract used by migration services.
type TargetPoolProvider interface {
	Pool(ctx context.Context, databaseID DatabaseID) (*pgxpool.Pool, error)
}

type ConnectionManager struct {
	repository RegistryRepository
	protector  CredentialProtector
	mu         sync.Mutex
	pools      map[DatabaseID]*pgxpool.Pool
}

func NewConnectionManager(repository RegistryRepository, protector CredentialProtector) *ConnectionManager {
	return &ConnectionManager{
		repository: repository,
		protector:  protector,
		pools:      make(map[DatabaseID]*pgxpool.Pool),
	}
}

func (m *ConnectionManager) Pool(ctx context.Context, databaseID DatabaseID) (*pgxpool.Pool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if pool := m.pools[databaseID]; pool != nil {
		return pool, nil
	}
	record, err := m.repository.FindByID(ctx, databaseID)
	if err != nil {
		return nil, err
	}
	password, err := m.protector.Open(record.PasswordCiphertext)
	if err != nil { return nil, fmt.Errorf("decrypt target password: %w", err) }
	record.Config.Password = password
	pool, err := newPool(ctx, record.Config)
	if err != nil {
		return nil, ErrDatabaseUnavailable
	}
	m.pools[databaseID] = pool
	return pool, nil
}

func (m *ConnectionManager) Invalidate(databaseID DatabaseID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pool := m.pools[databaseID]; pool != nil {
		pool.Close()
		delete(m.pools, databaseID)
	}
}

func (m *ConnectionManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, pool := range m.pools {
		pool.Close()
		delete(m.pools, id)
	}
}

func newPool(ctx context.Context, database DatabaseConfig) (*pgxpool.Pool, error) {
	dsn := targetDSN(database)
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	config.MaxConns = 10
	config.MinConns = 0
	config.MaxConnLifetime = 30 * time.Minute
	config.MaxConnIdleTime = 5 * time.Minute
	config.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func targetDSN(database DatabaseConfig) string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(database.Username, database.Password),
		Host:   net.JoinHostPort(database.Host, strconv.Itoa(int(database.Port))),
		Path:   database.DatabaseName,
	}
	query := u.Query()
	query.Set("sslmode", database.SSLMode)
	u.RawQuery = query.Encode()
	return u.String()
}
