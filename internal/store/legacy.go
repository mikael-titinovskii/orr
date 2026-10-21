package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"
)

// legacyStats is the shape of the stats.json file orr wrote before the database.
type legacyStats struct {
	Version          int                                  `json:"version"`
	DailyCosts       map[string]float64                   `json:"daily_costs,omitempty"`
	DailyTokens      map[string]int64                     `json:"daily_tokens,omitempty"`
	RequestCount     uint64                               `json:"request_count"`
	CreditSpend      float64                              `json:"credit_spend,omitempty"`
	CreditMonth      string                               `json:"credit_month,omitempty"`
	Budget           float64                              `json:"budget,omitempty"`
	BudgetReset      string                               `json:"budget_reset,omitempty"`
	ProviderPins     map[string]legacyPin                 `json:"provider_pins,omitempty"`
	BenchmarkRuns    map[string]time.Time                 `json:"benchmark_runs,omitempty"`
	RecoveryRuns     map[string]time.Time                 `json:"recovery_runs,omitempty"`
	TimeWeight       *float64                             `json:"time_weight,omitempty"`
	Degradation      *bool                                `json:"degradation,omitempty"`
	BenchmarkSamples map[string]map[string][]legacySample `json:"benchmark_samples,omitempty"`
}

type legacyPin struct {
	Provider string    `json:"provider"`
	PinnedAt time.Time `json:"pinned_at"`
}

type legacySample struct {
	Time   time.Time `json:"t"`
	TPS    float64   `json:"tps"`
	TTFTms float64   `json:"ttft_ms"`
}

const legacyVersion = 1

// ImportLegacyStats converts a pre-database stats.json into rows. It reports
// whether it imported anything: the file is left alone when the database
// already holds data, and renamed to <name>.imported after a successful import
// so the next start does not repeat it.
func (s *Store) ImportLegacyStats(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read legacy stats: %w", err)
	}
	populated, err := s.hasData()
	if err != nil {
		return false, err
	}
	if populated {
		return false, nil
	}

	var saved legacyStats
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return false, fmt.Errorf("decode legacy stats: top-level null is invalid")
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		return false, fmt.Errorf("decode legacy stats: %w", err)
	}
	// Version 0 is the pre-versioning format, byte-compatible with version 1.
	if saved.Version != 0 && saved.Version != legacyVersion {
		return false, fmt.Errorf("decode legacy stats: unsupported version %d (expected %d)", saved.Version, legacyVersion)
	}
	if saved.CreditSpend != 0 && saved.CreditMonth == "" {
		return false, fmt.Errorf("decode legacy stats: credit_month is required when credit_spend is set")
	}

	stats := Stats{
		DailyCosts:       saved.DailyCosts,
		DailyTokens:      saved.DailyTokens,
		RequestCount:     saved.RequestCount,
		CreditSpend:      saved.CreditSpend,
		CreditMonth:      saved.CreditMonth,
		Budget:           saved.Budget,
		BudgetReset:      saved.BudgetReset,
		ProviderPins:     make(map[string]Pin, len(saved.ProviderPins)),
		BenchmarkRuns:    saved.BenchmarkRuns,
		RecoveryRuns:     saved.RecoveryRuns,
		TimeWeight:       saved.TimeWeight,
		Degradation:      saved.Degradation,
		BenchmarkSamples: make(map[string]map[string][]Sample, len(saved.BenchmarkSamples)),
	}
	for model, pin := range saved.ProviderPins {
		stats.ProviderPins[model] = Pin{Provider: pin.Provider, PinnedAt: pin.PinnedAt}
	}
	for model, providers := range saved.BenchmarkSamples {
		pool := make(map[string][]Sample, len(providers))
		for provider, samples := range providers {
			converted := make([]Sample, len(samples))
			for i, sample := range samples {
				converted[i] = Sample{Time: sample.Time, TPS: sample.TPS, TTFTMs: sample.TTFTms}
			}
			pool[provider] = converted
		}
		stats.BenchmarkSamples[model] = pool
	}

	if err := s.SaveStats(stats); err != nil {
		return false, fmt.Errorf("import legacy stats: %w", err)
	}
	// The import is committed, so the source only needs to stop looking
	// importable. A stale backup from an earlier attempt must not block that on
	// Windows, where os.Rename refuses an existing target. Archiving is best
	// effort: the history is already in the database, and failing startup over a
	// file a scanner or indexer is holding would be worse than leaving it, which
	// only means the next start skips it as already superseded.
	imported := path + ".imported"
	if err := os.Remove(imported); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("imported %s, but could not replace %s: %v", path, imported, err)
		return true, nil
	}
	if err := os.Rename(path, imported); err != nil {
		log.Printf("imported %s, but could not rename it out of the way: %v", path, err)
		return true, nil
	}
	return true, nil
}

// hasData reports whether anything has been written to the aggregate tables. A
// database holding only routing rows still imports history: the routing schema
// carries no measurements of its own.
func (s *Store) hasData() (bool, error) {
	row := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM state) +
		(SELECT COUNT(*) FROM benchmark_samples) +
		(SELECT COUNT(*) FROM provider_pins) +
		(SELECT COUNT(*) FROM benchmark_runs) +
		(SELECT COUNT(*) FROM recovery_runs) +
		(SELECT COUNT(*) FROM daily_costs) +
		(SELECT COUNT(*) FROM daily_tokens)`)
	var count int
	if err := row.Scan(&count); err != nil {
		return false, fmt.Errorf("inspect database contents: %w", err)
	}
	return count > 0, nil
}
