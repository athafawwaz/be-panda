-- +goose Up
-- +goose StatementBegin
IF OBJECT_ID('dbo.upload_staging', 'U') IS NULL
BEGIN
    CREATE TABLE dbo.upload_staging (
        id NVARCHAR(64) NOT NULL PRIMARY KEY,
        user_id NVARCHAR(64) NOT NULL,
        badge_number NVARCHAR(50) NOT NULL,
        file_path NVARCHAR(500) NOT NULL,
        original_name NVARCHAR(255) NOT NULL,
        file_extension NVARCHAR(20) NOT NULL,
        file_size_bytes BIGINT NOT NULL,
        file_hash_sha256 NVARCHAR(64) NOT NULL,
        user_message NVARCHAR(1000) NULL,
        suggestion_json NVARCHAR(MAX) NULL,
        created_at DATETIME2 NOT NULL DEFAULT SYSDATETIME(),
        expires_at DATETIME2 NOT NULL
    );
END

IF COL_LENGTH('dbo.documents', 'upload_channel') IS NULL
    ALTER TABLE dbo.documents ADD upload_channel NVARCHAR(20) NOT NULL
        CONSTRAINT df_documents_upload_channel DEFAULT N'MANUAL';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE dbo.documents DROP CONSTRAINT IF EXISTS df_documents_upload_channel;
ALTER TABLE dbo.documents DROP COLUMN IF EXISTS upload_channel;
DROP TABLE IF EXISTS dbo.upload_staging;
-- +goose StatementEnd
