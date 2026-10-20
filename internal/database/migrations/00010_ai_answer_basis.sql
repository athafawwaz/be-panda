-- Label basis jawaban Agentic AI (docs/AGENTIC_AI_PLAN.md §4): DOCUMENTS (mengutip dokumen), GENERAL (pengetahuan
-- umum, tanpa dokumen), atau MIXED. NULL = jawaban yang tersimpan sebelum fitur ini ada.

-- +goose Up
-- +goose StatementBegin
IF COL_LENGTH('dbo.ai_chat_messages', 'answer_basis') IS NULL
    ALTER TABLE dbo.ai_chat_messages ADD answer_basis NVARCHAR(20) NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE dbo.ai_chat_messages DROP COLUMN IF EXISTS answer_basis;
-- +goose StatementEnd
