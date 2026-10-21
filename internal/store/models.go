package store

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// Model is one model's routing configuration. It mirrors the provider block orr
// sends upstream — hence the JSON tags — plus the bookkeeping it keeps for
// itself, which never leaves the process.
type Model struct {
	Order             []string  `json:"order,omitempty"`
	Only              []string  `json:"only,omitempty"`
	Ignore            []string  `json:"ignore,omitempty"`
	AllowFallbacks    *bool     `json:"allow_fallbacks,omitempty"`
	RequireParameters *bool     `json:"require_parameters,omitempty"`
	DataCollection    string    `json:"data_collection,omitempty"`
	ZDR               *bool     `json:"zdr,omitempty"`
	Sort              string    `json:"sort,omitempty"`
	ManualPin         string    `json:"-"`
	UpdatedAt         time.Time `json:"-"`
	Blocked           []Blocked `json:"-"`
}

// Blocked is a provider OpenRouter refused to route to for this account.
type Blocked struct {
	Provider   string
	Reason     string
	DetectedAt time.Time
}

// Models returns every configured model keyed by its OpenRouter id.
func (s *Store) Models() (map[string]Model, error) {
	models := make(map[string]Model)
	rows, err := s.db.Query(`SELECT model, allow_fallbacks, require_parameters, data_collection, zdr, sort, manual_pin, updated_at FROM models ORDER BY model`)
	if err != nil {
		return nil, fmt.Errorf("read models: %w", err)
	}
	for rows.Next() {
		var (
			name              string
			allowFallbacks    sql.NullInt64
			requireParameters sql.NullInt64
			dataCollection    string
			zdr               sql.NullInt64
			sortValue         string
			manualPin         string
			updatedAt         string
		)
		if err := rows.Scan(&name, &allowFallbacks, &requireParameters, &dataCollection, &zdr, &sortValue, &manualPin, &updatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan model: %w", err)
		}
		models[name] = Model{
			AllowFallbacks:    nullableBool(allowFallbacks),
			RequireParameters: nullableBool(requireParameters),
			DataCollection:    dataCollection,
			ZDR:               nullableBool(zdr),
			Sort:              sortValue,
			ManualPin:         manualPin,
			UpdatedAt:         decodeTime(updatedAt),
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read models: %w", err)
	}
	rows.Close()

	for _, table := range []struct {
		name   string
		assign func(*Model, []string)
	}{
		{"model_order", func(m *Model, values []string) { m.Order = values }},
		{"model_only", func(m *Model, values []string) { m.Only = values }},
		{"model_ignore", func(m *Model, values []string) { m.Ignore = values }},
	} {
		values, err := s.readProviderLists(table.name)
		if err != nil {
			return nil, err
		}
		for model, list := range values {
			entry, ok := models[model]
			if !ok {
				continue
			}
			table.assign(&entry, list)
			models[model] = entry
		}
	}

	blocked, err := s.readBlocked()
	if err != nil {
		return nil, err
	}
	for model, list := range blocked {
		entry, ok := models[model]
		if !ok {
			continue
		}
		entry.Blocked = list
		models[model] = entry
	}
	return models, nil
}

func (s *Store) readProviderLists(table string) (map[string][]string, error) {
	rows, err := s.db.Query(fmt.Sprintf(`SELECT model, provider FROM %s ORDER BY model, position`, table))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", table, err)
	}
	defer rows.Close()
	values := make(map[string][]string)
	for rows.Next() {
		var model, provider string
		if err := rows.Scan(&model, &provider); err != nil {
			return nil, fmt.Errorf("scan %s: %w", table, err)
		}
		values[model] = append(values[model], provider)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", table, err)
	}
	return values, nil
}

func (s *Store) readBlocked() (map[string][]Blocked, error) {
	rows, err := s.db.Query(`SELECT model, provider, reason, detected_at FROM model_blocked ORDER BY model, detected_at, provider`)
	if err != nil {
		return nil, fmt.Errorf("read model_blocked: %w", err)
	}
	defer rows.Close()
	blocked := make(map[string][]Blocked)
	for rows.Next() {
		var model, provider, reason, detectedAt string
		if err := rows.Scan(&model, &provider, &reason, &detectedAt); err != nil {
			return nil, fmt.Errorf("scan model_blocked: %w", err)
		}
		blocked[model] = append(blocked[model], Blocked{
			Provider:   provider,
			Reason:     reason,
			DetectedAt: decodeTime(detectedAt),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read model_blocked: %w", err)
	}
	return blocked, nil
}

// SaveModels replaces the stored routing configuration with models. It runs in
// one transaction: a failure leaves the previous configuration intact.
func (s *Store) SaveModels(models map[string]Model) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin model write: %w", err)
	}
	defer tx.Rollback()

	for _, table := range []string{"models", "model_order", "model_only", "model_ignore", "model_blocked"} {
		if _, err := tx.Exec("DELETE FROM " + table); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}

	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		entry := models[name]
		if _, err := tx.Exec(
			`INSERT INTO models (model, allow_fallbacks, require_parameters, data_collection, zdr, sort, manual_pin, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			name,
			bindBool(entry.AllowFallbacks),
			bindBool(entry.RequireParameters),
			entry.DataCollection,
			bindBool(entry.ZDR),
			entry.Sort,
			entry.ManualPin,
			encodeTime(entry.UpdatedAt),
		); err != nil {
			return fmt.Errorf("write model %s: %w", name, err)
		}
		for _, list := range []struct {
			table   string
			entries []string
		}{
			{"model_order", entry.Order},
			{"model_only", entry.Only},
			{"model_ignore", entry.Ignore},
		} {
			for position, provider := range list.entries {
				if _, err := tx.Exec(
					"INSERT INTO "+list.table+" (model, position, provider) VALUES (?, ?, ?)",
					name, position, provider,
				); err != nil {
					return fmt.Errorf("write %s for %s: %w", list.table, name, err)
				}
			}
		}
		for _, entry := range entry.Blocked {
			if _, err := tx.Exec(
				`INSERT INTO model_blocked (model, provider, reason, detected_at) VALUES (?, ?, ?, ?)`,
				name, entry.Provider, entry.Reason, encodeTime(entry.DetectedAt),
			); err != nil {
				return fmt.Errorf("write model_blocked for %s: %w", name, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit model write: %w", err)
	}
	return nil
}

func bindBool(value *bool) any {
	if value == nil {
		return nil
	}
	if *value {
		return int64(1)
	}
	return int64(0)
}

func nullableBool(value sql.NullInt64) *bool {
	if !value.Valid {
		return nil
	}
	parsed := value.Int64 != 0
	return &parsed
}

// encodeTime writes a timestamp as RFC 3339 with its original offset. The
// offset must survive the round trip: callers compare calendar days
// (Format(time.DateOnly)), so normalising to UTC would move an evening timestamp
// to the previous day for anyone east of Greenwich and make a fresh catalog look
// stale.
func encodeTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format(time.RFC3339Nano)
}

func decodeTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
