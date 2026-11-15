-- +goose Up
ALTER TABLE state ADD COLUMN prediction_tokens INTEGER;

-- +goose Down
ALTER TABLE state DROP COLUMN prediction_tokens;
