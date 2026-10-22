-- +goose Up
CREATE TABLE IF NOT EXISTS upload_staging (
    id VARCHAR(64) NOT NULL PRIMARY KEY,
    user_id VARCHAR(64) NOT NULL,
    badge_number VARCHAR(50) NOT NULL,
    file_path VARCHAR(500) NOT NULL,
    original_name VARCHAR(255) NOT NULL,
    file_extension VARCHAR(20) NOT NULL,
    file_size_bytes BIGINT NOT NULL,
    file_hash_sha256 VARCHAR(64) NOT NULL,
    user_message VARCHAR(1000) NULL,
    suggestion_json TEXT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

ALTER TABLE documents ADD COLUMN IF NOT EXISTS upload_channel VARCHAR(20) NOT NULL DEFAULT 'MANUAL';

-- +goose Down
ALTER TABLE documents DROP COLUMN IF EXISTS upload_channel;
DROP TABLE IF EXISTS upload_staging;
