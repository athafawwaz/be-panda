-- Ekstraksi isi PDF diperbaiki (spasi, urutan huruf font CID, titik daftar isi):
-- tandai semua dokumen agar dibaca ulang oleh ReindexPendingDocuments saat backend start.

-- +goose Up
-- +goose StatementBegin
UPDATE dbo.documents SET content_indexed_at = NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
