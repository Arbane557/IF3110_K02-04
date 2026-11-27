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
	"wbd/backend/observability"
)

// TargetPoolProvider is the connection contract used by migration services.
type TargetPoolProvider interface {
	Pool(ctx context.Context, databaseID DatabaseID) (*pgxpool.Pool, error)
}

type ConnectionManager struct {
	repository RegistryRepository
	protector  CredentialProtector
	mu         sync.Mutex
	poolsMu    sync.RWMutex
	pools      map[DatabaseID]*pgxpool.Pool
	observer   observability.Observer
}

func NewConnectionManager(repository RegistryRepository, protector CredentialProtector, observers ...observability.Observer) *ConnectionManager {
	manager := &ConnectionManager{
		repository: repository,
		protector:  protector,
		pools:      make(map[DatabaseID]*pgxpool.Pool),
	}
	if len(observers) > 0 {
		manager.observer = observers[0]
	}
	return manager
}

func (m *ConnectionManager) Pool(ctx context.Context, databaseID DatabaseID) (*pgxpool.Pool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.poolsMu.RLock()
	cached := m.pools[databaseID]
	m.poolsMu.RUnlock()
	if pool := cached; pool != nil {
		return pool, nil
	}
	record, err := m.repository.FindByID(ctx, databaseID)
	if err != nil {
		return nil, err
	}
	password, err := m.protector.Open(record.PasswordCiphertext)
	if err != nil {
		return nil, fmt.Errorf("decrypt target password: %w", err)
	}
	record.Config.Password = password
	pool, err := newPool(ctx, record.Config, m.observer)
	if err != nil {
		return nil, ErrDatabaseUnavailable
	}
	m.poolsMu.Lock()
	m.pools[databaseID] = pool
	m.poolsMu.Unlock()
	return pool, nil
}

func (m *ConnectionManager) Invalidate(databaseID DatabaseID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.poolsMu.Lock()
	pool := m.pools[databaseID]
	delete(m.pools, databaseID)
	m.poolsMu.Unlock()
	if pool != nil {
		pool.Close()
	}
}

func (m *ConnectionManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.poolsMu.Lock()
	pools := m.pools
	m.pools = make(map[DatabaseID]*pgxpool.Pool)
	m.poolsMu.Unlock()
	for _, pool := range pools {
		pool.Close()
	}
}

func newPool(ctx context.Context, database DatabaseConfig, observers ...observability.Observer) (*pgxpool.Pool, error) {
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
	if len(observers) > 0 && observers[0] != nil {
		config.ConnConfig.Tracer = poolAcquireTracer{observer: observers[0]}
	}

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

func (m *ConnectionManager) PoolStats() (active, idle int32) {
	// Opening or closing a target may block under mu. Scrapes only need the
	// short-lived map lock and must remain available during that operation.
	m.poolsMu.RLock()
	defer m.poolsMu.RUnlock()
	for _, pool := range m.pools {
		stats := pool.Stat()
		active += stats.AcquiredConns()
		idle += stats.IdleConns()
	}
	return
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
