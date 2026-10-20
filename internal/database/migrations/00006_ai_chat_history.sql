-- +goose Up
-- +goose StatementBegin
IF OBJECT_ID('dbo.ai_chat_sessions', 'U') IS NULL
BEGIN
    CREATE TABLE dbo.ai_chat_sessions (
        id NVARCHAR(64) NOT NULL PRIMARY KEY,
        user_id NVARCHAR(64) NOT NULL,
        title NVARCHAR(200) NOT NULL,
        message_count INT NOT NULL DEFAULT 0,
        created_at DATETIME2 NOT NULL,
        updated_at DATETIME2 NOT NULL
    );
    CREATE INDEX idx_ai_chat_sessions_user_updated ON dbo.ai_chat_sessions (user_id, updated_at DESC);
END

IF OBJECT_ID('dbo.ai_chat_messages', 'U') IS NULL
BEGIN
    CREATE TABLE dbo.ai_chat_messages (
        id BIGINT IDENTITY(1,1) NOT NULL PRIMARY KEY,
        session_id NVARCHAR(64) NOT NULL,
        prompt NVARCHAR(MAX) NOT NULL,
        filter_module NVARCHAR(30) NULL,
        reply_markdown NVARCHAR(MAX) NULL,
        engine_mode NVARCHAR(200) NULL,
        scope_label NVARCHAR(300) NULL,
        matched_documents_json NVARCHAR(MAX) NULL,
        created_at DATETIME2 NOT NULL
    );
    CREATE INDEX idx_ai_chat_messages_session ON dbo.ai_chat_messages (session_id, id);
END
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS dbo.ai_chat_messages;
DROP TABLE IF EXISTS dbo.ai_chat_sessions;
-- +goose StatementEnd
