package store

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// Pin is an automatic provider pin: which provider sticky routing chose for a
// model, and when. Manual pins live on Model.ManualPin because the user owns
// them.
type Pin struct {
	Provider string
	PinnedAt time.Time
}

// Sample is one benchmark round's measurement of a provider.
type Sample struct {
	Time   time.Time
	TPS    float64
	TTFTMs float64
}

// Stats is the aggregate measurement state the dashboard and ranking restore on
// startup. Request records are not part of it: they live in memory for the
// process lifetime.
type Stats struct {
	DailyCosts       map[string]float64
	DailyTokens      map[string]int64
	RequestCount     uint64
	CreditSpend      float64
	CreditMonth      string
	Budget           float64
	BudgetReset      string
	ProviderPins     map[string]Pin
	BenchmarkRuns    map[string]time.Time
	RecoveryRuns     map[string]time.Time
	TimeWeight       *float64
	Degradation      *bool
	BenchmarkSamples map[string]map[string][]Sample
}

// LoadStats reads the aggregate measurement state. Empty tables yield empty
// maps, never nil ones, so callers can write into them.
func (s *Store) LoadStats() (Stats, error) {
	stats := Stats{
		DailyCosts:       make(map[string]float64),
		DailyTokens:      make(map[string]int64),
		ProviderPins:     make(map[string]Pin),
		BenchmarkRuns:    make(map[string]time.Time),
		RecoveryRuns:     make(map[string]time.Time),
		BenchmarkSamples: make(map[string]map[string][]Sample),
	}

	if err := s.loadState(&stats); err != nil {
		return Stats{}, err
	}
	if err := s.loadTotals(&stats); err != nil {
		return Stats{}, err
	}
	if err := s.loadPins(&stats); err != nil {
		return Stats{}, err
	}
	if err := s.loadRuns("benchmark_runs", "ran_at", stats.BenchmarkRuns); err != nil {
		return Stats{}, err
	}
	if err := s.loadRuns("recovery_runs", "ran_at", stats.RecoveryRuns); err != nil {
		return Stats{}, err
	}
	if err := s.loadSamples(&stats); err != nil {
		return Stats{}, err
	}
	return stats, nil
}

func (s *Store) loadState(stats *Stats) error {
	row := s.db.QueryRow(`SELECT request_count, credit_spend, credit_month, budget, budget_reset, time_weight, degradation FROM state WHERE id = 1`)
	var (
		timeWeight  sql.NullFloat64
		degradation sql.NullInt64
	)
	err := row.Scan(&stats.RequestCount, &stats.CreditSpend, &stats.CreditMonth, &stats.Budget, &stats.BudgetReset, &timeWeight, &degradation)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	if timeWeight.Valid {
		value := timeWeight.Float64
		stats.TimeWeight = &value
	}
	if degradation.Valid {
		value := degradation.Int64 != 0
		stats.Degradation = &value
	}
	return nil
}

func (s *Store) loadTotals(stats *Stats) error {
	costs, err := s.db.Query(`SELECT day, cost FROM daily_costs`)
	if err != nil {
		return fmt.Errorf("read daily_costs: %w", err)
	}
	for costs.Next() {
		var day string
		var cost float64
		if err := costs.Scan(&day, &cost); err != nil {
			costs.Close()
			return fmt.Errorf("scan daily_costs: %w", err)
		}
		stats.DailyCosts[day] = cost
	}
	if err := costs.Err(); err != nil {
		costs.Close()
		return fmt.Errorf("read daily_costs: %w", err)
	}
	costs.Close()

	tokens, err := s.db.Query(`SELECT day, tokens FROM daily_tokens`)
	if err != nil {
		return fmt.Errorf("read daily_tokens: %w", err)
	}
	defer tokens.Close()
	for tokens.Next() {
		var day string
		var count int64
		if err := tokens.Scan(&day, &count); err != nil {
			return fmt.Errorf("scan daily_tokens: %w", err)
		}
		stats.DailyTokens[day] = count
	}
	if err := tokens.Err(); err != nil {
		return fmt.Errorf("read daily_tokens: %w", err)
	}
	return nil
}

func (s *Store) loadPins(stats *Stats) error {
	rows, err := s.db.Query(`SELECT model, provider, pinned_at FROM provider_pins`)
	if err != nil {
		return fmt.Errorf("read provider_pins: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var model, provider, pinnedAt string
		if err := rows.Scan(&model, &provider, &pinnedAt); err != nil {
			return fmt.Errorf("scan provider_pins: %w", err)
		}
		stats.ProviderPins[model] = Pin{Provider: provider, PinnedAt: decodeTime(pinnedAt)}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read provider_pins: %w", err)
	}
	return nil
}

func (s *Store) loadRuns(table, column string, target map[string]time.Time) error {
	rows, err := s.db.Query(fmt.Sprintf(`SELECT model, %s FROM %s`, column, table))
	if err != nil {
		return fmt.Errorf("read %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var model, timestamp string
		if err := rows.Scan(&model, &timestamp); err != nil {
			return fmt.Errorf("scan %s: %w", table, err)
		}
		target[model] = decodeTime(timestamp)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read %s: %w", table, err)
	}
	return nil
}

func (s *Store) loadSamples(stats *Stats) error {
	rows, err := s.db.Query(`SELECT model, provider, measured_at, tps, ttft_ms FROM benchmark_samples ORDER BY model, provider, measured_at`)
	if err != nil {
		return fmt.Errorf("read benchmark_samples: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var model, provider, measuredAt string
		var tps, ttftMs float64
		if err := rows.Scan(&model, &provider, &measuredAt, &tps, &ttftMs); err != nil {
			return fmt.Errorf("scan benchmark_samples: %w", err)
		}
		if stats.BenchmarkSamples[model] == nil {
			stats.BenchmarkSamples[model] = make(map[string][]Sample)
		}
		stats.BenchmarkSamples[model][provider] = append(stats.BenchmarkSamples[model][provider], Sample{
			Time:   decodeTime(measuredAt),
			TPS:    tps,
			TTFTMs: ttftMs,
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read benchmark_samples: %w", err)
	}
	return nil
}

// SaveStats replaces the aggregate measurement state in one transaction.
func (s *Store) SaveStats(stats Stats) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin stats write: %w", err)
	}
	defer tx.Rollback()

	for _, table := range []string{"daily_costs", "daily_tokens", "provider_pins", "benchmark_runs", "recovery_runs", "benchmark_samples"} {
		if _, err := tx.Exec("DELETE FROM " + table); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}

	if err := saveState(tx, stats); err != nil {
		return err
	}

	for _, day := range sortedKeys(stats.DailyCosts) {
		if _, err := tx.Exec(`INSERT INTO daily_costs (day, cost) VALUES (?, ?)`, day, stats.DailyCosts[day]); err != nil {
			return fmt.Errorf("write daily_costs: %w", err)
		}
	}
	for _, day := range sortedKeys(stats.DailyTokens) {
		if _, err := tx.Exec(`INSERT INTO daily_tokens (day, tokens) VALUES (?, ?)`, day, stats.DailyTokens[day]); err != nil {
			return fmt.Errorf("write daily_tokens: %w", err)
		}
	}
	for _, model := range sortedKeys(stats.ProviderPins) {
		pin := stats.ProviderPins[model]
		if _, err := tx.Exec(`INSERT INTO provider_pins (model, provider, pinned_at) VALUES (?, ?, ?)`,
			model, pin.Provider, encodeTime(pin.PinnedAt)); err != nil {
			return fmt.Errorf("write provider_pins: %w", err)
		}
	}
	if err := saveRuns(tx, "benchmark_runs", stats.BenchmarkRuns); err != nil {
		return err
	}
	if err := saveRuns(tx, "recovery_runs", stats.RecoveryRuns); err != nil {
		return err
	}
	if err := saveSamples(tx, stats.BenchmarkSamples); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit stats write: %w", err)
	}
	return nil
}

func saveState(tx *sql.Tx, stats Stats) error {
	var timeWeight, degradation any
	if stats.TimeWeight != nil {
		timeWeight = *stats.TimeWeight
	}
	if stats.Degradation != nil {
		degradation = boolAsInt(*stats.Degradation)
	}
	_, err := tx.Exec(
		`INSERT INTO state (id, request_count, credit_spend, credit_month, budget, budget_reset, time_weight, degradation)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET
		     request_count = excluded.request_count,
		     credit_spend  = excluded.credit_spend,
		     credit_month  = excluded.credit_month,
		     budget        = excluded.budget,
		     budget_reset  = excluded.budget_reset,
		     time_weight   = excluded.time_weight,
		     degradation   = excluded.degradation`,
		stats.RequestCount, stats.CreditSpend, stats.CreditMonth,
		stats.Budget, stats.BudgetReset, timeWeight, degradation,
	)
	if err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return nil
}

func saveRuns(tx *sql.Tx, table string, runs map[string]time.Time) error {
	for _, model := range sortedKeys(runs) {
		if _, err := tx.Exec("INSERT INTO "+table+" (model, ran_at) VALUES (?, ?)", model, encodeTime(runs[model])); err != nil {
			return fmt.Errorf("write %s: %w", table, err)
		}
	}
	return nil
}

func saveSamples(tx *sql.Tx, samples map[string]map[string][]Sample) error {
	models := make([]string, 0, len(samples))
	for model := range samples {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		providers := sortedKeys(samples[model])
		for _, provider := range providers {
			for _, sample := range samples[model][provider] {
				if _, err := tx.Exec(
					`INSERT INTO benchmark_samples (model, provider, measured_at, tps, ttft_ms) VALUES (?, ?, ?, ?, ?)`,
					model, provider, encodeTime(sample.Time), sample.TPS, sample.TTFTMs,
				); err != nil {
					return fmt.Errorf("write benchmark_samples: %w", err)
				}
			}
		}
	}
	return nil
}

func boolAsInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
