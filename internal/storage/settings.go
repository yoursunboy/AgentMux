package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SettingInstallID is the key holding this installation's identifier.
//
// It is generated once and kept, so that a future client can tell one
// AgentMux installation from another without any account system.
const SettingInstallID = "server.installId"

// ErrSettingNotFound is returned when a key has never been set.
var ErrSettingNotFound = errors.New("storage: setting not found")

// SettingStore stores installation-level key/value settings.
type SettingStore struct {
	db *sql.DB
}

// Get returns a setting value, or ErrSettingNotFound.
func (r *SettingStore) Get(ctx context.Context, key string) (string, error) {
	var value string
	err := r.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrSettingNotFound
	case err != nil:
		return "", fmt.Errorf("storage: read setting %q: %w", key, err)
	}
	return value, nil
}

// Set stores a setting value, replacing any previous value.
func (r *SettingStore) Set(ctx context.Context, key, value string) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, formatTime(time.Now()),
	)
	if err != nil {
		return fmt.Errorf("storage: write setting %q: %w", key, err)
	}
	return nil
}

// GetOrCreate returns the existing value for key, or generates, stores, and
// returns a new one.
//
// generate is called outside the write, so a caller that produces an
// identifier does not hold a write transaction while it does so.
func (r *SettingStore) GetOrCreate(ctx context.Context, key string, generate func() (string, error)) (string, error) {
	value, err := r.Get(ctx, key)
	if err == nil {
		return value, nil
	}
	if !errors.Is(err, ErrSettingNotFound) {
		return "", err
	}
	if generate == nil {
		return "", fmt.Errorf("storage: setting %q is missing and no generator was supplied", key)
	}

	generated, err := generate()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(generated) == "" {
		return "", fmt.Errorf("storage: generator for setting %q returned an empty value", key)
	}
	if err := r.Set(ctx, key, generated); err != nil {
		return "", err
	}
	return generated, nil
}
