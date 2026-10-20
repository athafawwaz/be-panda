-- +goose Up
-- +goose StatementBegin
IF OBJECT_ID('dbo.master_teams', 'U') IS NULL
BEGIN
    CREATE TABLE dbo.master_teams (
        id NVARCHAR(64) NOT NULL PRIMARY KEY,
        team_code NVARCHAR(50) NOT NULL UNIQUE,
        team_name NVARCHAR(150) NOT NULL,
        has_branches BIT NOT NULL DEFAULT 0,
        created_at DATETIME2 NOT NULL DEFAULT SYSDATETIME()
    );
END

IF OBJECT_ID('dbo.master_branches', 'U') IS NULL
BEGIN
    CREATE TABLE dbo.master_branches (
        id NVARCHAR(64) NOT NULL PRIMARY KEY,
        team_id NVARCHAR(64) NOT NULL,
        branch_code NVARCHAR(50) NOT NULL,
        branch_name NVARCHAR(150) NOT NULL,
        created_at DATETIME2 NOT NULL DEFAULT SYSDATETIME()
    );
END

IF OBJECT_ID('dbo.users', 'U') IS NULL
BEGIN
    CREATE TABLE dbo.users (
        id NVARCHAR(64) NOT NULL PRIMARY KEY,
        badge_number NVARCHAR(50) NOT NULL UNIQUE,
        full_name NVARCHAR(150) NOT NULL,
        email NVARCHAR(150) NOT NULL,
        password_hash NVARCHAR(255) NOT NULL DEFAULT N'Panda',
        role NVARCHAR(30) NOT NULL,
        team_id NVARCHAR(64) NULL,
        branch_id NVARCHAR(64) NULL,
        is_active BIT NOT NULL DEFAULT 1,
        created_at DATETIME2 NOT NULL DEFAULT SYSDATETIME(),
        updated_at DATETIME2 NOT NULL DEFAULT SYSDATETIME()
    );
END

IF OBJECT_ID('dbo.app_folders', 'U') IS NULL
BEGIN
    CREATE TABLE dbo.app_folders (
        id NVARCHAR(64) NOT NULL PRIMARY KEY,
        folder_name NVARCHAR(200) NOT NULL,
        description NVARCHAR(500) NULL,
        assigned_team_id NVARCHAR(64) NOT NULL,
        assigned_branch_id NVARCHAR(64) NULL,
        created_by_badge NVARCHAR(50) NOT NULL,
        created_by_name NVARCHAR(150) NOT NULL,
        created_at DATETIME2 NOT NULL DEFAULT SYSDATETIME()
    );
END

IF OBJECT_ID('dbo.applications', 'U') IS NULL
BEGIN
    CREATE TABLE dbo.applications (
        id NVARCHAR(64) NOT NULL PRIMARY KEY,
        folder_id NVARCHAR(64) NOT NULL,
        app_code NVARCHAR(50) NOT NULL UNIQUE,
        app_name NVARCHAR(200) NOT NULL,
        description NVARCHAR(1000) NULL,
        status NVARCHAR(50) NOT NULL DEFAULT N'PRODUCTION',
        created_by_badge NVARCHAR(50) NOT NULL,
        created_by_name NVARCHAR(150) NOT NULL,
        created_at DATETIME2 NOT NULL DEFAULT SYSDATETIME()
    );
END

IF OBJECT_ID('dbo.app_change_sections', 'U') IS NULL
BEGIN
    CREATE TABLE dbo.app_change_sections (
        id NVARCHAR(64) NOT NULL PRIMARY KEY,
        application_id NVARCHAR(64) NOT NULL,
        version_label NVARCHAR(50) NOT NULL,
        section_title NVARCHAR(200) NOT NULL,
        change_description NVARCHAR(1000) NULL,
        is_initial_release BIT NOT NULL DEFAULT 0,
        sort_order INT NOT NULL DEFAULT 1,
        release_date NVARCHAR(30) NULL,
        created_by_badge NVARCHAR(50) NOT NULL,
        created_by_name NVARCHAR(150) NOT NULL,
        created_at DATETIME2 NOT NULL DEFAULT SYSDATETIME()
    );
END

IF OBJECT_ID('dbo.documents', 'U') IS NULL
BEGIN
    CREATE TABLE dbo.documents (
        id NVARCHAR(64) NOT NULL PRIMARY KEY,
        module_source NVARCHAR(30) NOT NULL,
        folder_id NVARCHAR(64) NULL,
        application_id NVARCHAR(64) NULL,
        section_id NVARCHAR(64) NULL,
        doc_category NVARCHAR(50) NOT NULL,
        it_domain NVARCHAR(100) NULL,
        title NVARCHAR(255) NOT NULL,
        standardized_name NVARCHAR(255) NOT NULL,
        original_filename NVARCHAR(255) NOT NULL,
        file_extension NVARCHAR(20) NOT NULL,
        file_path NVARCHAR(500) NOT NULL,
        file_size_bytes BIGINT NOT NULL DEFAULT 0,
        file_hash_sha256 NVARCHAR(64) NULL,
        version_label NVARCHAR(50) NOT NULL DEFAULT N'v1.0',
        audit_tags NVARCHAR(500) NULL,
        valid_until NVARCHAR(30) NULL,
        validity_status NVARCHAR(30) NOT NULL DEFAULT N'VALID',
        visibility_scope NVARCHAR(30) NOT NULL DEFAULT N'TEAM_ONLY',
        owner_team_id NVARCHAR(64) NULL,
        owner_branch_id NVARCHAR(64) NULL,
        ai_summary NVARCHAR(MAX) NULL,
        extracted_text NVARCHAR(MAX) NULL,
        uploaded_by_badge NVARCHAR(50) NOT NULL,
        uploaded_by_name NVARCHAR(150) NOT NULL,
        uploaded_by_role NVARCHAR(30) NOT NULL,
        uploaded_by_team_label NVARCHAR(200) NULL,
        uploaded_at DATETIME2 NOT NULL DEFAULT SYSDATETIME()
    );
END

IF OBJECT_ID('dbo.document_chunks', 'U') IS NULL
BEGIN
    CREATE TABLE dbo.document_chunks (
        id NVARCHAR(64) NOT NULL PRIMARY KEY,
        document_id NVARCHAR(64) NOT NULL,
        chunk_index INT NOT NULL,
        chunk_text NVARCHAR(MAX) NOT NULL,
        created_at DATETIME2 NOT NULL DEFAULT SYSDATETIME()
    );
END

IF OBJECT_ID('dbo.upload_audit_logs', 'U') IS NULL
BEGIN
    CREATE TABLE dbo.upload_audit_logs (
        id BIGINT IDENTITY(1,1) PRIMARY KEY,
        badge_number NVARCHAR(50) NOT NULL,
        user_name NVARCHAR(150) NOT NULL,
        user_role NVARCHAR(30) NOT NULL,
        team_and_branch_label NVARCHAR(200) NULL,
        action_type NVARCHAR(50) NOT NULL,
        module_source NVARCHAR(30) NULL,
        document_id NVARCHAR(64) NULL,
        folder_name NVARCHAR(200) NULL,
        app_name NVARCHAR(200) NULL,
        section_version NVARCHAR(100) NULL,
        document_title NVARCHAR(255) NULL,
        visibility_label NVARCHAR(150) NULL,
        detail_description NVARCHAR(1000) NOT NULL,
        action_timestamp DATETIME2 NOT NULL DEFAULT SYSDATETIME()
    );
END
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS dbo.upload_audit_logs;
DROP TABLE IF EXISTS dbo.document_chunks;
DROP TABLE IF EXISTS dbo.documents;
DROP TABLE IF EXISTS dbo.app_change_sections;
DROP TABLE IF EXISTS dbo.applications;
DROP TABLE IF EXISTS dbo.app_folders;
DROP TABLE IF EXISTS dbo.users;
DROP TABLE IF EXISTS dbo.master_branches;
DROP TABLE IF EXISTS dbo.master_teams;
-- +goose StatementEnd
