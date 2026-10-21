package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

func runUpdate(envPath string, maxProviders int, cacheOnly bool, output io.Writer) error {
	return runUpdateWithOptions(envPath, false, maxProviders, cacheOnly, output)
}

func runUpdateWithOptions(envPath string, preferEnvFile bool, maxProviders int, cacheOnly bool, output io.Writer) error {
	if maxProviders < 1 {
		return errors.New("max must be at least 1")
	}

	cfg, err := loadConfigWithOptions(envPath, preferEnvFile)
	if err != nil {
		return err
	}
	apiKey := cfg.openRouterKey()

	client := &http.Client{Timeout: 20 * time.Second}
	models := make([]string, 0, len(cfg.Models))
	for model := range cfg.Models {
		models = append(models, model)
	}
	if len(models) == 0 {
		return errors.New("providers.yaml has no models to update")
	}
	sort.Strings(models)

	// Fetch every model's endpoints concurrently with a bounded worker pool so
	// updating many models does not serialize on network round trips.
	orders := make(map[string][]string, len(models))
	skips := make(map[string]string)
	failures := make(map[string]error)
	results := make(chan updateResult, len(models))
	var wg sync.WaitGroup
	workers := min(4, len(models))
	sem := make(chan struct{}, workers)
	for _, model := range models {
		wg.Add(1)
		go func(model string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results <- fetchUpdateOrder(client, cfg.Upstream, apiKey, model, maxProviders, cacheOnly)
		}(model)
	}
	wg.Wait()
	close(results)
	for result := range results {
		switch {
		case result.err != nil:
			failures[result.model] = result.err
		case result.skipped != "":
			skips[result.model] = result.skipped
		default:
			orders[result.model] = result.order
		}
	}

	replaceOrders(&cfg, orders)
	if err := writeProvidersFileAtomic(cfg.providersPath, cfg.Models); err != nil {
		return err
	}
	for _, model := range models {
		if order, ok := orders[model]; ok {
			fmt.Fprintf(output, "%s: %s\n", model, strings.Join(order, ", "))
		} else if reason, ok := skips[model]; ok {
			fmt.Fprintf(output, "%s: skipped (%s)\n", model, reason)
		}
	}
	var errs []error
	for _, model := range models {
		if err, ok := failures[model]; ok {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type updateResult struct {
	model   string
	order   []string
	skipped string
	err     error
}

func fetchUpdateOrder(client *http.Client, upstream, apiKey, model string, maxProviders int, cacheOnly bool) updateResult {
	if isAliasModel(model) {
		return updateResult{model: model, skipped: "alias model"}
	}
	result, err := fetchEndpoints(context.Background(), client, upstream, apiKey, model)
	if err != nil {
		return updateResult{model: model, err: fmt.Errorf("update %s: %w", model, err)}
	}
	compatible := compatibleEndpoints(result.Data.Endpoints)
	endpoints := rankUpdateEndpoints(compatible, cacheOnly)
	if len(endpoints) == 0 {
		switch {
		case len(result.Data.Endpoints) > 0 && !supportsTools(result.Data.Endpoints):
			return updateResult{model: model, skipped: "no tool-capable endpoints"}
		case cacheOnly && len(compatible) > 0:
			return updateResult{model: model, err: fmt.Errorf("update %s: no cache-capable endpoints found", model)}
		default:
			return updateResult{model: model, err: fmt.Errorf("update %s: no healthy tool-capable endpoints found", model)}
		}
	}
	limit := min(maxProviders, len(endpoints))
	order := make([]string, limit)
	for i := range limit {
		order[i] = endpoints[i].Tag
	}
	return updateResult{model: model, order: order}
}

func rankUpdateEndpoints(endpoints []modelEndpoint, cacheOnly bool) []modelEndpoint {
	return rankEndpoints(endpoints, cacheOnly)
}

func replaceOrders(cfg *config, orders map[string][]string) {
	for model, order := range orders {
		routing := cfg.Models[model]
		routing.Order = append([]string(nil), order...)
		routing.UpdatedAt = time.Now()
		if routing.ManualPin != "" && !containsString(routing.Order, routing.ManualPin) {
			routing.Order = append(routing.Order, routing.ManualPin)
		}
		cfg.Models[model] = routing
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
