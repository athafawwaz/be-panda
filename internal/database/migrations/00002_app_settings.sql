-- +goose Up
CREATE TABLE IF NOT EXISTS app_settings (
    setting_key VARCHAR(100) NOT NULL PRIMARY KEY,
    setting_value TEXT NULL,
    is_secret BOOLEAN NOT NULL DEFAULT FALSE,
    updated_by_badge VARCHAR(50) NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS app_settings;
