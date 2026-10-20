-- +goose Up
-- +goose StatementBegin
IF COL_LENGTH('dbo.documents', 'content_indexed_at') IS NULL
    ALTER TABLE dbo.documents ADD content_indexed_at DATETIME2 NULL;

IF COL_LENGTH('dbo.documents', 'content_chunk_count') IS NULL
    ALTER TABLE dbo.documents ADD content_chunk_count INT NOT NULL
        CONSTRAINT df_documents_content_chunk_count DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE dbo.documents DROP CONSTRAINT IF EXISTS df_documents_content_chunk_count;
ALTER TABLE dbo.documents DROP COLUMN IF EXISTS content_chunk_count, content_indexed_at;
-- +goose StatementEnd
