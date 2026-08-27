package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/yty-build/acp-gateway/internal/settings"
)

// PutProviderSettings inserts or replaces one validated non-secret provider
// configuration. Composition must supply argv from a registered connector
// profile; Settings UI is not an argv-authoring surface.
func (store *Store) PutProviderSettings(ctx context.Context, provider settings.Provider) error {
	provider = provider.Clone()
	if err := provider.Validate(); err != nil {
		return err
	}
	configJSON, err := json.Marshal(provider.LaunchConfig)
	if err != nil {
		return fmt.Errorf("encode launch configuration: %w", err)
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return ErrClosed
	}
	_, err = store.db.ExecContext(ctx, `
		INSERT INTO provider_settings(
			provider_id, enabled, probe_status, launch_config_json,
			can_manager, can_subordinate, can_resume, can_replay
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider_id) DO UPDATE SET
			enabled = excluded.enabled,
			probe_status = excluded.probe_status,
			launch_config_json = excluded.launch_config_json,
			can_manager = excluded.can_manager,
			can_subordinate = excluded.can_subordinate,
			can_resume = excluded.can_resume,
			can_replay = excluded.can_replay`,
		string(provider.ID), boolInt(provider.Enabled), string(provider.ProbeStatus), string(configJSON),
		boolInt(provider.Capabilities.Manager), boolInt(provider.Capabilities.Subordinate),
		boolInt(provider.Capabilities.Resume), boolInt(provider.Capabilities.Replay),
	)
	err = classifyWriteError(contextError(ctx, err))
	return store.afterWrite(ctx, err)
}

// GetProviderSettings returns configuration for one exact provider ID.
func (store *Store) GetProviderSettings(ctx context.Context, id settings.ProviderID) (settings.Provider, error) {
	if err := id.Validate(); err != nil {
		return settings.Provider{}, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return settings.Provider{}, ErrClosed
	}
	provider, err := scanProvider(store.db.QueryRowContext(ctx, providerSelect+` WHERE provider_id = ?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return settings.Provider{}, ErrNotFound
	}
	return provider, contextError(ctx, err)
}

// ListProviderSettings returns all provider configuration ordered by provider ID.
func (store *Store) ListProviderSettings(ctx context.Context) ([]settings.Provider, error) {
	return store.listProviderSettings(ctx, "", nil)
}

// ListEnabledProviders returns explicitly enabled providers with one conformed capability.
func (store *Store) ListEnabledProviders(ctx context.Context, capability settings.Capability) ([]settings.Provider, error) {
	if err := capability.Validate(); err != nil {
		return nil, err
	}
	var column string
	switch capability {
	case settings.CapabilityManager:
		column = "can_manager"
	case settings.CapabilitySubordinate:
		column = "can_subordinate"
	case settings.CapabilityResume:
		column = "can_resume"
	case settings.CapabilityReplay:
		column = "can_replay"
	}
	return store.listProviderSettings(ctx, ` WHERE enabled = 1 AND `+column+` = 1`, nil)
}

func (store *Store) listProviderSettings(ctx context.Context, where string, args []any) ([]settings.Provider, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return nil, ErrClosed
	}
	rows, err := store.db.QueryContext(ctx, providerSelect+where+` ORDER BY provider_id ASC`, args...)
	if err != nil {
		return nil, contextError(ctx, err)
	}
	defer rows.Close()
	var providers []settings.Provider
	for rows.Next() {
		provider, err := scanProvider(rows)
		if err != nil {
			return nil, contextError(ctx, err)
		}
		providers = append(providers, provider)
	}
	if err := rows.Err(); err != nil {
		return nil, contextError(ctx, err)
	}
	return providers, nil
}

// SetProviderEnabled changes only explicit new-Session eligibility.
func (store *Store) SetProviderEnabled(ctx context.Context, id settings.ProviderID, enabled bool) error {
	if err := id.Validate(); err != nil {
		return err
	}
	return store.updateProviderSetting(ctx, `enabled = ?`, boolInt(enabled), id)
}

// SetProviderProbeStatus changes only the latest finite mechanical probe result.
func (store *Store) SetProviderProbeStatus(ctx context.Context, id settings.ProviderID, status settings.ProbeStatus) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if err := status.Validate(); err != nil {
		return err
	}
	return store.updateProviderSetting(ctx, `probe_status = ?`, string(status), id)
}

func (store *Store) updateProviderSetting(ctx context.Context, assignment string, value any, id settings.ProviderID) error {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return ErrClosed
	}
	result, err := store.db.ExecContext(ctx, `UPDATE provider_settings SET `+assignment+` WHERE provider_id = ?`, value, string(id))
	if err == nil {
		var changed int64
		changed, err = result.RowsAffected()
		if err == nil && changed == 0 {
			err = ErrNotFound
		}
	}
	err = classifyWriteError(contextError(ctx, err))
	return store.afterWrite(ctx, err)
}

// DeleteProviderSettings removes only TaskAI connector configuration. Existing
// Session metadata and provider-owned state are intentionally unrelated.
func (store *Store) DeleteProviderSettings(ctx context.Context, id settings.ProviderID) error {
	if err := id.Validate(); err != nil {
		return err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return ErrClosed
	}
	result, err := store.db.ExecContext(ctx, `DELETE FROM provider_settings WHERE provider_id = ?`, string(id))
	if err == nil {
		var changed int64
		changed, err = result.RowsAffected()
		if err == nil && changed == 0 {
			err = ErrNotFound
		}
	}
	err = classifyWriteError(contextError(ctx, err))
	return store.afterWrite(ctx, err)
}

const providerSelect = `
	SELECT provider_id, enabled, probe_status, launch_config_json,
		can_manager, can_subordinate, can_resume, can_replay
	FROM provider_settings`

func scanProvider(row rowScanner) (settings.Provider, error) {
	var (
		id, status, configJSON                        string
		enabled, manager, subordinate, resume, replay int64
	)
	if err := row.Scan(&id, &enabled, &status, &configJSON, &manager, &subordinate, &resume, &replay); err != nil {
		return settings.Provider{}, err
	}
	var config settings.LaunchConfig
	decoder := json.NewDecoder(bytes.NewReader([]byte(configJSON)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return settings.Provider{}, fmt.Errorf("decode launch configuration: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return settings.Provider{}, errors.New("decode launch configuration: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return settings.Provider{}, fmt.Errorf("decode launch configuration trailing data: %w", err)
	}
	provider := settings.Provider{
		ID:           settings.ProviderID(id),
		Enabled:      enabled == 1,
		ProbeStatus:  settings.ProbeStatus(status),
		LaunchConfig: config,
		Capabilities: settings.Capabilities{
			Manager:     manager == 1,
			Subordinate: subordinate == 1,
			Resume:      resume == 1,
			Replay:      replay == 1,
		},
	}
	if err := provider.Validate(); err != nil {
		return settings.Provider{}, fmt.Errorf("invalid provider Settings in database: %w", err)
	}
	return provider, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
