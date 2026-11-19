-- Ekstraksi isi PDF diperbaiki (spasi, urutan huruf font CID, titik daftar isi):
-- tandai semua dokumen agar dibaca ulang oleh ReindexPendingDocuments saat backend start.

-- +goose Up
UPDATE documents SET content_indexed_at = NULL;

-- +goose Down
SELECT 1;
