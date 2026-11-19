-- Label basis jawaban Agentic AI (docs/AGENTIC_AI_PLAN.md §4): DOCUMENTS (mengutip dokumen), GENERAL (pengetahuan
-- umum, tanpa dokumen), atau MIXED. NULL = jawaban yang tersimpan sebelum fitur ini ada.

-- +goose Up
ALTER TABLE ai_chat_messages ADD COLUMN IF NOT EXISTS answer_basis VARCHAR(20) NULL;

-- +goose Down
ALTER TABLE ai_chat_messages DROP COLUMN IF EXISTS answer_basis;
