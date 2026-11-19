-- Riwayat chat Agentic AI menyimpan jejak langkah AI (mode Thinking: pencarian, baca, rerank), pemakaian token,
-- dan pesan yang dibalas / diteruskan user, agar tetap tampil saat percakapan dibuka ulang.

-- +goose Up
ALTER TABLE ai_chat_messages ADD COLUMN IF NOT EXISTS trace_json TEXT NULL;
ALTER TABLE ai_chat_messages ADD COLUMN IF NOT EXISTS reply_to_json TEXT NULL;

-- +goose Down
ALTER TABLE ai_chat_messages DROP COLUMN IF EXISTS trace_json, DROP COLUMN IF EXISTS reply_to_json;
