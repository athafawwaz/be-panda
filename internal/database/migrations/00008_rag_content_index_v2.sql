-- Indeks isi dokumen v2 (docs/RAG_IMPROVEMENT_PLAN.md §3.1, §3.2, R3a, R8):
-- versi pipeline indeks, jumlah halaman & halaman tak terbaca, serta indeks kata per chunk.
-- Dokumen lama otomatis diindeks ulang oleh ReindexPendingDocuments karena content_index_version = 0.

-- +goose Up
-- +goose StatementBegin
IF COL_LENGTH('dbo.documents', 'content_index_version') IS NULL
    ALTER TABLE dbo.documents ADD content_index_version INT NOT NULL
        CONSTRAINT df_documents_content_index_version DEFAULT 0;

IF COL_LENGTH('dbo.documents', 'content_page_count') IS NULL
    ALTER TABLE dbo.documents ADD content_page_count INT NULL;

IF COL_LENGTH('dbo.documents', 'content_unread_page_count') IS NULL
    ALTER TABLE dbo.documents ADD content_unread_page_count INT NOT NULL
        CONSTRAINT df_documents_content_unread_page_count DEFAULT 0;

IF COL_LENGTH('dbo.document_chunks', 'token_count') IS NULL
    ALTER TABLE dbo.document_chunks ADD token_count INT NOT NULL
        CONSTRAINT df_document_chunks_token_count DEFAULT 0;

IF OBJECT_ID('dbo.document_chunk_terms', 'U') IS NULL
BEGIN
    CREATE TABLE dbo.document_chunk_terms (
        chunk_id NVARCHAR(64) NOT NULL,
        term NVARCHAR(64) NOT NULL,
        document_id NVARCHAR(64) NOT NULL,
        tf INT NOT NULL,
        CONSTRAINT pk_document_chunk_terms PRIMARY KEY (chunk_id, term)
    );
    CREATE INDEX idx_document_chunk_terms_term ON dbo.document_chunk_terms (term, document_id) INCLUDE (chunk_id, tf);
    CREATE INDEX idx_document_chunk_terms_document ON dbo.document_chunk_terms (document_id);
END
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS dbo.document_chunk_terms;
ALTER TABLE dbo.document_chunks DROP CONSTRAINT IF EXISTS df_document_chunks_token_count;
ALTER TABLE dbo.document_chunks DROP COLUMN IF EXISTS token_count;
ALTER TABLE dbo.documents DROP CONSTRAINT IF EXISTS df_documents_content_index_version;
ALTER TABLE dbo.documents DROP CONSTRAINT IF EXISTS df_documents_content_unread_page_count;
ALTER TABLE dbo.documents DROP COLUMN IF EXISTS content_index_version, content_page_count, content_unread_page_count;
-- +goose StatementEnd
