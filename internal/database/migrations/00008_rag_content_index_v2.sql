-- Indeks isi dokumen v2 (docs/RAG_IMPROVEMENT_PLAN.md §3.1, §3.2, R3a, R8):
-- versi pipeline indeks, jumlah halaman & halaman tak terbaca, serta indeks kata per chunk.
-- Dokumen lama otomatis diindeks ulang oleh ReindexPendingDocuments karena content_index_version = 0.

-- +goose Up
ALTER TABLE documents ADD COLUMN IF NOT EXISTS content_index_version INT NOT NULL DEFAULT 0;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS content_page_count INT NULL;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS content_unread_page_count INT NOT NULL DEFAULT 0;
ALTER TABLE document_chunks ADD COLUMN IF NOT EXISTS token_count INT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS document_chunk_terms (
    chunk_id VARCHAR(64) NOT NULL,
    term VARCHAR(64) NOT NULL,
    document_id VARCHAR(64) NOT NULL,
    tf INT NOT NULL,
    CONSTRAINT pk_document_chunk_terms PRIMARY KEY (chunk_id, term)
);
CREATE INDEX IF NOT EXISTS idx_document_chunk_terms_term ON document_chunk_terms (term, document_id) INCLUDE (chunk_id, tf);
CREATE INDEX IF NOT EXISTS idx_document_chunk_terms_document ON document_chunk_terms (document_id);

-- +goose Down
DROP TABLE IF EXISTS document_chunk_terms;
ALTER TABLE document_chunks DROP COLUMN IF EXISTS token_count;
ALTER TABLE documents
    DROP COLUMN IF EXISTS content_index_version,
    DROP COLUMN IF EXISTS content_page_count,
    DROP COLUMN IF EXISTS content_unread_page_count;
