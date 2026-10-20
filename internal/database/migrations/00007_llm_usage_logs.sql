-- +goose Up
-- +goose StatementBegin
IF OBJECT_ID('dbo.llm_usage_logs', 'U') IS NULL
BEGIN
    CREATE TABLE dbo.llm_usage_logs (
        id BIGINT IDENTITY(1,1) NOT NULL PRIMARY KEY,
        provider NVARCHAR(30) NOT NULL,
        model NVARCHAR(150) NOT NULL,
        key_fingerprint NVARCHAR(16) NOT NULL,
        status_code INT NOT NULL,
        success BIT NOT NULL,
        prompt_tokens INT NOT NULL DEFAULT 0,
        completion_tokens INT NOT NULL DEFAULT 0,
        quota_limit INT NULL,
        retry_until DATETIME2 NULL,
        error_message NVARCHAR(500) NULL,
        created_at DATETIME2 NOT NULL
    );
    CREATE INDEX idx_llm_usage_logs_key_model_time ON dbo.llm_usage_logs (key_fingerprint, model, created_at);
END
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS dbo.llm_usage_logs;
-- +goose StatementEnd
