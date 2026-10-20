-- Riwayat chat Agentic AI menyimpan jejak langkah AI (mode Thinking: pencarian, baca, rerank), pemakaian token,
-- dan pesan yang dibalas / diteruskan user, agar tetap tampil saat percakapan dibuka ulang.

-- +goose Up
-- +goose StatementBegin
IF COL_LENGTH('dbo.ai_chat_messages', 'trace_json') IS NULL
    ALTER TABLE dbo.ai_chat_messages ADD trace_json NVARCHAR(MAX) NULL;

IF COL_LENGTH('dbo.ai_chat_messages', 'reply_to_json') IS NULL
    ALTER TABLE dbo.ai_chat_messages ADD reply_to_json NVARCHAR(MAX) NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE dbo.ai_chat_messages DROP COLUMN IF EXISTS trace_json, reply_to_json;
-- +goose StatementEnd
