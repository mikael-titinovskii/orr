package app

import "time"

// restoreAutomaticPin reveals the automatic route hidden by a manual override.
func restoreAutomaticPin(routing *routingState, stats *stats, model string) {
	routing.pinActionsMu.Lock()
	defer routing.pinActionsMu.Unlock()
	// Another client may have installed a new override after the caller cleared
	// its own. Never replace that newer manual choice with the saved automatic pin.
	if cfg, ok := routing.modelConfig(model); !ok || cfg.ManualPin != "" {
		return
	}
	if pin, ok := stats.providerPinsSnapshot()[model]; ok {
		routing.restorePins(map[string]persistedPin{model: pin})
		if provider, manual := routing.pinInfo(model); provider != "" && !manual {
			return
		}
		stats.clearAutoPin(model)
		// Do not let an unresolved saved pin replace the fallback selected below
		// if endpoint discovery completes later.
		routing.unpin(model)
	}

	cfg, ok := routing.modelConfig(model)
	if !ok {
		return
	}
	providers := cfg.Order
	if len(providers) == 0 {
		providers = cfg.Only
	}
	pool := stats.benchmarkPool(model)
	now := routing.now()
	cutoff := now.Add(-routing.pinTTL)
	blocked := routing.blockedProviders(model)
	apiErrors := providerAPIErrorRates(stats.snapshot().Records, model, now)
	for _, provider := range providers {
		if _, refused := blocked[provider]; refused || apiErrors[provider] > 0 {
			continue
		}
		var measuredAt time.Time
		for _, sample := range pool[provider] {
			if sample.TPS > 0 && !sample.Time.Before(cutoff) && sample.Time.After(measuredAt) {
				measuredAt = sample.Time
			}
		}
		if measuredAt.IsZero() {
			continue
		}
		pin := persistedPin{Provider: provider, PinnedAt: measuredAt}
		stats.setAutoPinAt(model, provider, measuredAt)
		routing.restorePins(map[string]persistedPin{model: pin})
		return
	}

	provider := routing.active(model, stats.snapshot().Observed[model])
	if routing.ensureAutoPin(model, provider) {
		stats.setAutoPin(model, provider)
	}
}
