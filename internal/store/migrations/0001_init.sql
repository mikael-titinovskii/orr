-- +goose Up
CREATE TABLE models (
    model              TEXT PRIMARY KEY,
    allow_fallbacks    INTEGER,
    require_parameters INTEGER,
    data_collection    TEXT NOT NULL DEFAULT '',
    zdr                INTEGER,
    sort               TEXT NOT NULL DEFAULT '',
    manual_pin         TEXT NOT NULL DEFAULT '',
    updated_at         TEXT NOT NULL DEFAULT ''
);

CREATE TABLE model_order (
    model    TEXT NOT NULL,
    position INTEGER NOT NULL,
    provider TEXT NOT NULL,
    PRIMARY KEY (model, position)
);

CREATE TABLE model_only (
    model    TEXT NOT NULL,
    position INTEGER NOT NULL,
    provider TEXT NOT NULL,
    PRIMARY KEY (model, position)
);

CREATE TABLE model_ignore (
    model    TEXT NOT NULL,
    position INTEGER NOT NULL,
    provider TEXT NOT NULL,
    PRIMARY KEY (model, position)
);

CREATE TABLE model_blocked (
    model       TEXT NOT NULL,
    provider    TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    detected_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (model, provider)
);

CREATE TABLE provider_pins (
    model     TEXT PRIMARY KEY,
    provider  TEXT NOT NULL,
    pinned_at TEXT NOT NULL
);

CREATE TABLE daily_costs (
    day  TEXT PRIMARY KEY,
    cost REAL NOT NULL
);

CREATE TABLE daily_tokens (
    day    TEXT PRIMARY KEY,
    tokens INTEGER NOT NULL
);

CREATE TABLE state (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    request_count INTEGER NOT NULL DEFAULT 0,
    credit_spend  REAL    NOT NULL DEFAULT 0,
    credit_month  TEXT    NOT NULL DEFAULT '',
    budget        REAL    NOT NULL DEFAULT 0,
    budget_reset  TEXT    NOT NULL DEFAULT '',
    time_weight   REAL,
    degradation   INTEGER
);

CREATE TABLE benchmark_runs (
    model  TEXT PRIMARY KEY,
    ran_at TEXT NOT NULL
);

CREATE TABLE recovery_runs (
    model  TEXT PRIMARY KEY,
    ran_at TEXT NOT NULL
);

CREATE TABLE benchmark_samples (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    model       TEXT NOT NULL,
    provider    TEXT NOT NULL,
    measured_at TEXT NOT NULL,
    tps         REAL NOT NULL,
    ttft_ms     REAL NOT NULL
);

CREATE INDEX benchmark_samples_pool ON benchmark_samples (model, provider, measured_at);

-- +goose Down
DROP TABLE benchmark_samples;
DROP TABLE recovery_runs;
DROP TABLE benchmark_runs;
DROP TABLE state;
DROP TABLE daily_tokens;
DROP TABLE daily_costs;
DROP TABLE provider_pins;
DROP TABLE model_blocked;
DROP TABLE model_ignore;
DROP TABLE model_only;
DROP TABLE model_order;
DROP TABLE models;
