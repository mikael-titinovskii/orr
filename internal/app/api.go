package app

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type managementAPI struct {
	p      *proxy
	mu     sync.Mutex
	jobs   []apiJob
	nextID uint64
	closed bool
	wg     sync.WaitGroup
}

type apiJob struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Model      string     `json:"model"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Error      *string    `json:"error"`
}

type apiModel struct {
	ID             string   `json:"id"`
	Order          []string `json:"order"`
	Providers      []string `json:"providers"`
	ActiveProvider *string  `json:"active_provider"`
	ManualPin      *string  `json:"manual_pin"`
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optionalMeasurement(v float64) *float64 {
	if v <= 0 {
		return nil
	}
	return &v
}

func (a *managementAPI) model(id string) apiModel {
	cfg, _ := a.p.routing.modelConfig(id)
	snap := a.p.stats.snapshot()
	return apiModel{ID: id, Order: append([]string{}, cfg.Order...),
		Providers:      append([]string{}, a.p.routing.providers(id, snap.Observed[id])...),
		ActiveProvider: optionalString(a.p.routing.active(id, snap.Observed[id])),
		ManualPin:      optionalString(cfg.ManualPin)}
}

func apiJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *managementAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	allowed := ""
	switch path {
	case "stats", "models", "model":
		allowed = "GET"
	case "pin":
		allowed = "PUT, DELETE"
	case "refresh", "benchmarks":
		allowed = "POST"
	default:
		if strings.HasPrefix(path, "jobs/") {
			allowed = "GET"
		}
	}
	if allowed == "" {
		writeError(w, 404, "unknown management resource")
		return
	}
	if !containsString(strings.Split(allowed, ", "), r.Method) {
		w.Header().Set("Allow", allowed)
		writeError(w, 405, "method not allowed")
		return
	}
	if strings.HasPrefix(path, "jobs/") {
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, job := range a.jobs {
			if job.ID == strings.TrimPrefix(path, "jobs/") {
				apiJSON(w, 200, job)
				return
			}
		}
		writeError(w, 404, "unknown job")
		return
	}
	if path == "stats" {
		s := a.p.stats.snapshot()
		apiJSON(w, 200, map[string]any{
			"request_count": s.RequestCount, "session_request_count": s.SessionRequestCount,
			"in_flight": s.InFlight, "daily_costs_usd": s.DailyCosts, "daily_tokens": s.DailyTokens,
			"average_ttft_ms":           optionalMeasurement(float64(s.AverageTTFT) / float64(time.Millisecond)),
			"average_tokens_per_second": optionalMeasurement(s.AverageTPS),
			"latency_p50_ms":            optionalMeasurement(float64(s.LatencyP50) / float64(time.Millisecond)),
			"latency_p95_ms":            optionalMeasurement(float64(s.LatencyP95) / float64(time.Millisecond)),
		})
		return
	}
	if path == "models" {
		ids := make([]string, 0)
		for id := range a.p.routing.modelsSnapshot() {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		models := make([]apiModel, 0, len(ids))
		for _, id := range ids {
			models = append(models, a.model(id))
		}
		apiJSON(w, 200, map[string]any{"models": models})
		return
	}
	model := r.URL.Query().Get("model")
	if !validModelID(model) || len(r.URL.Query()["model"]) != 1 {
		writeError(w, 400, "one author/model parameter is required")
		return
	}
	if _, ok := a.p.routing.modelConfig(model); !ok {
		writeError(w, 404, "unknown model")
		return
	}
	if path == "model" {
		apiJSON(w, 200, a.model(model))
		return
	}
	if path == "pin" {
		provider := ""
		if r.Method == http.MethodPut {
			media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if media != "application/json" {
				writeError(w, 415, "expected application/json")
				return
			}
			var body struct {
				Provider string `json:"provider"`
			}
			r.Body = http.MaxBytesReader(w, r.Body, 4096)
			dec := json.NewDecoder(r.Body)
			dec.DisallowUnknownFields()
			err := dec.Decode(&body)
			if err == nil {
				var extra any
				if next := dec.Decode(&extra); next != io.EOF {
					err = next
					if err == nil {
						err = errors.New("trailing JSON")
					}
				}
			}
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					writeError(w, 413, "body exceeds 4 KiB")
				} else {
					writeError(w, 400, "invalid pin body")
				}
				return
			}
			provider = body.Provider
			if provider == "" || !containsString(a.model(model).Providers, provider) {
				writeError(w, 400, "unknown provider for model")
				return
			}
		}
		// DELETE is idempotent and must not clear an existing automatic pin.
		cfg, _ := a.p.routing.modelConfig(model)
		if provider != "" || cfg.ManualPin != "" {
			if err := a.p.routing.setManualPin(model, provider); err != nil {
				writeError(w, 500, "could not persist manual pin")
				return
			}
			if provider == "" {
				restoreAutomaticPin(a.p.routing, a.p.stats, model)
			}
		}
		apiJSON(w, 200, a.model(model))
		return
	}
	if r.ContentLength != 0 {
		writeError(w, 400, "job submission requires an empty body")
		return
	}
	a.submit(w, model, path)
}

func (a *managementAPI) submit(w http.ResponseWriter, model, kind string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		writeError(w, 503, "server is shutting down")
		return
	}
	for _, job := range a.jobs {
		if job.Status != "running" {
			continue
		}
		if job.Model == model && job.Kind == kind {
			w.Header().Set("Location", "/api/v1/jobs/"+job.ID)
			apiJSON(w, 202, job)
		} else {
			writeError(w, 409, "another management job is running")
		}
		return
	}
	a.nextID++
	job := apiJob{ID: strconv.FormatUint(a.nextID, 10), Model: model, Kind: kind, Status: "running", CreatedAt: time.Now().UTC()}
	if len(a.jobs) == 64 {
		a.jobs = a.jobs[1:]
	}
	a.jobs = append(a.jobs, job)
	a.wg.Add(1)
	go a.run(job)
	w.Header().Set("Location", "/api/v1/jobs/"+job.ID)
	apiJSON(w, 202, job)
}

func (a *managementAPI) run(job apiJob) {
	defer a.wg.Done()
	var err error
	if job.Kind == "refresh" {
		client := *a.p.client
		client.Timeout = 30 * time.Second
		err = a.p.routing.refreshModelOrder(job.Model, &client, a.p.cfg.Upstream, a.p.cfg.openRouterKey(), a.p.cfg.UpdateMaxProviders, a.p.cfg.UpdateCacheOnly)
	} else {
		revision := a.p.routing.modelRevision(job.Model)
		providers := a.model(job.Model).Providers
		results, complete := a.p.benchmarkProviders(job.Model, providers, a.p.now().Add(manualBenchmarkGate), false, 4)
		err = a.p.applyProviderTestResults(job.Model, results, revision)
		success := false
		for _, result := range results {
			if result.err == nil {
				success = true
			}
		}
		if !complete || !success {
			err = errors.New("benchmark incomplete")
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.jobs {
		if a.jobs[i].ID != job.ID {
			continue
		}
		now := time.Now().UTC()
		a.jobs[i].FinishedAt = &now
		a.jobs[i].Status = "succeeded"
		if err != nil {
			a.jobs[i].Status = "failed"
			a.jobs[i].Error = optionalString("operation failed; retry or inspect current model state")
		}
	}
}

func (a *managementAPI) stop() {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
}
