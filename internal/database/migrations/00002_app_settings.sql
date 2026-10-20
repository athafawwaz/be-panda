-- +goose Up
-- +goose StatementBegin
IF OBJECT_ID('dbo.app_settings', 'U') IS NULL
BEGIN
    CREATE TABLE dbo.app_settings (
        setting_key NVARCHAR(100) NOT NULL PRIMARY KEY,
        setting_value NVARCHAR(MAX) NULL,
        is_secret BIT NOT NULL DEFAULT 0,
        updated_by_badge NVARCHAR(50) NULL,
        updated_at DATETIME2 NOT NULL DEFAULT SYSDATETIME()
    );
END
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS dbo.app_settings;
-- +goose StatementEnd
