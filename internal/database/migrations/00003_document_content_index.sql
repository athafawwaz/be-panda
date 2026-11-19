-- +goose Up
ALTER TABLE documents ADD COLUMN IF NOT EXISTS content_indexed_at TIMESTAMPTZ NULL;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS content_chunk_count INT NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE documents DROP COLUMN IF EXISTS content_chunk_count, DROP COLUMN IF EXISTS content_indexed_at;
