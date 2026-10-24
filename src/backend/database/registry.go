package database

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
)

// CredentialProtector keeps target credentials encrypted at rest. The key is
// owned by application configuration, never by the registry table.
type CredentialProtector interface {
	Seal(plaintext string) ([]byte, error)
	Open(ciphertext []byte) (string, error)
}

type aesGCMProtector struct{ aead cipher.AEAD }

func NewAESGCMProtector(key []byte) (CredentialProtector, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create credential cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create credential cipher: %w", err)
	}
	return aesGCMProtector{aead: aead}, nil
}

func (p aesGCMProtector) Seal(plaintext string) ([]byte, error) {
	nonce := make([]byte, p.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return p.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

func (p aesGCMProtector) Open(ciphertext []byte) (string, error) {
	if len(ciphertext) < p.aead.NonceSize() {
		return "", errors.New("invalid encrypted credential")
	}
	nonce, ciphertext := ciphertext[:p.aead.NonceSize()], ciphertext[p.aead.NonceSize():]
	plaintext, err := p.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", errors.New("cannot decrypt credential")
	}
	return string(plaintext), nil
}

type RegistryService struct {
	repository RegistryRepository
	manager    *ConnectionManager
	protector  CredentialProtector
}

func NewRegistryService(repository RegistryRepository, manager *ConnectionManager, protector CredentialProtector) *RegistryService {
	return &RegistryService{repository: repository, manager: manager, protector: protector}
}

func (s *RegistryService) Create(ctx context.Context, config DatabaseConfig) (DatabaseConfig, error) {
	if len(config.AccessRoles) == 0 {
		config.AccessRoles = []string{RoleAdmin}
	}
	if config.SSLMode == "" {
		config.SSLMode = "require"
	}
	if err := validateConfig(config, true); err != nil {
		return DatabaseConfig{}, err
	}
	ciphertext, err := s.protector.Seal(config.Password)
	if err != nil {
		return DatabaseConfig{}, fmt.Errorf("protect credential: %w", err)
	}
	record, err := s.repository.Create(ctx, databaseRecord{Config: withoutPassword(config), PasswordCiphertext: ciphertext})
	if err != nil {
		return DatabaseConfig{}, err
	}
	return record.Config, nil
}

func (s *RegistryService) List(ctx context.Context) ([]DatabaseSummary, error) {
	return s.repository.List(ctx)
}

func (s *RegistryService) ListByRole(ctx context.Context, role string) ([]DatabaseSummary, error) {
	return s.repository.ListByRole(ctx, role)
}

func (s *RegistryService) Get(ctx context.Context, id DatabaseID) (DatabaseConfig, error) {
	record, err := s.repository.FindByID(ctx, id)
	if err != nil {
		return DatabaseConfig{}, err
	}
	return record.Config, nil
}

func (s *RegistryService) Update(ctx context.Context, config DatabaseConfig) (DatabaseConfig, error) {
	if config.ID <= 0 {
		return DatabaseConfig{}, ErrInvalidConfig
	}
	if len(config.AccessRoles) == 0 {
		config.AccessRoles = []string{RoleAdmin}
	}
	if config.SSLMode == "" {
		config.SSLMode = "require"
	}
	current, err := s.repository.FindByID(ctx, config.ID)
	if err != nil {
		return DatabaseConfig{}, err
	}
	if err := validateConfig(config, false); err != nil {
		return DatabaseConfig{}, err
	}
	ciphertext := current.PasswordCiphertext
	if config.Password != "" {
		ciphertext, err = s.protector.Seal(config.Password)
		if err != nil {
			return DatabaseConfig{}, fmt.Errorf("protect credential: %w", err)
		}
	}
	record, err := s.repository.Update(ctx, databaseRecord{Config: withoutPassword(config), PasswordCiphertext: ciphertext})
	if err != nil {
		return DatabaseConfig{}, err
	}
	s.manager.Invalidate(config.ID)
	return record.Config, nil
}

func (s *RegistryService) Delete(ctx context.Context, id DatabaseID) error {
	if id <= 0 {
		return ErrNotFound
	}
	if err := s.repository.Delete(ctx, id); err != nil {
		return err
	}
	s.manager.Invalidate(id)
	return nil
}

func (s *RegistryService) TestConnection(ctx context.Context, id DatabaseID) error {
	pool, err := s.manager.Pool(ctx, id)
	if err != nil {
		return err
	}
	if err := pool.Ping(ctx); err != nil {
		s.manager.Invalidate(id)
		return ErrDatabaseUnavailable
	}
	return nil
}

func validateConfig(config DatabaseConfig, passwordRequired bool) error {
	if strings.TrimSpace(config.Name) == "" || strings.TrimSpace(config.Host) == "" ||
		strings.TrimSpace(config.DatabaseName) == "" || strings.TrimSpace(config.Username) == "" ||
		config.Port == 0 {
		return ErrInvalidConfig
	}
	if passwordRequired && config.Password == "" {
		return ErrInvalidConfig
	}
	for _, role := range config.AccessRoles {
		if role != RoleAdmin && role != RoleUser {
			return ErrInvalidConfig
		}
	}
	switch config.SSLMode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
	default:
		return ErrInvalidConfig
	}
	return nil
}

func withoutPassword(config DatabaseConfig) DatabaseConfig {
	config.Password = ""
	return config
}
