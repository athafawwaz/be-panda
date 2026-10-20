package models

import "time"

// Role akses pengguna
const (
	RoleSuperAdmin = "SUPER_ADMIN" // Pengembang: akses penuh + impersonate segmen, Docs, & setting AI
	RoleAdmin      = "ADMIN"       // Akses semua tim & kelola Folder / Master Akses
	RolePegawaiTI  = "PEGAWAI_TI"  // Terisolasi sesuai Tim & Branch
)

func IsValidRole(role string) bool {
	return role == RoleSuperAdmin || role == RoleAdmin || role == RolePegawaiTI
}

// DocCategories adalah kategori dokumen yang valid (urutan = urutan tampilan).
var DocCategories = []string{"PRD", "UAT", "BAST", "FSD", "SOP", "SLA", "DRC", "PENTEST", "LAINNYA"}

type MasterTeam struct {
	ID          string         `gorm:"primaryKey;type:nvarchar(64)" json:"id"`
	TeamCode    string         `gorm:"index;type:nvarchar(50);not null" json:"team_code"`
	TeamName    string         `gorm:"type:nvarchar(150);not null" json:"team_name"`
	HasBranches bool           `gorm:"not null;default:false" json:"has_branches"`
	CreatedAt   time.Time      `gorm:"autoCreateTime" json:"created_at"`
	Branches    []MasterBranch `gorm:"foreignKey:TeamID" json:"branches"`
}

func (MasterTeam) TableName() string { return "master_teams" }

type MasterBranch struct {
	ID         string    `gorm:"primaryKey;type:nvarchar(64)" json:"id"`
	TeamID     string    `gorm:"index;type:nvarchar(64);not null" json:"team_id"`
	BranchCode string    `gorm:"type:nvarchar(50);not null" json:"branch_code"`
	BranchName string    `gorm:"type:nvarchar(150);not null" json:"branch_name"`
	CreatedAt  time.Time `gorm:"autoCreateTime" json:"created_at"`
}

func (MasterBranch) TableName() string { return "master_branches" }

type User struct {
	ID           string        `gorm:"primaryKey;type:nvarchar(64)" json:"id"`
	BadgeNumber  string        `gorm:"index;type:nvarchar(50);not null" json:"badge_number"`
	FullName     string        `gorm:"type:nvarchar(150);not null" json:"full_name"`
	Email        string        `gorm:"type:nvarchar(150);not null" json:"email"`
	PasswordHash string        `gorm:"type:nvarchar(255);not null;default:'Panda'" json:"-"`
	Role         string        `gorm:"type:nvarchar(30);not null" json:"role"` // SUPER_ADMIN | ADMIN | PEGAWAI_TI
	TeamID       *string       `gorm:"type:nvarchar(64)" json:"team_id"`
	BranchID     *string       `gorm:"type:nvarchar(64)" json:"branch_id"`
	IsActive     bool          `gorm:"not null;default:true" json:"is_active"`
	CreatedAt    time.Time     `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt    time.Time     `gorm:"autoUpdateTime" json:"updated_at"`
	Team         *MasterTeam   `gorm:"foreignKey:TeamID" json:"team,omitempty"`
	Branch       *MasterBranch `gorm:"foreignKey:BranchID" json:"branch,omitempty"`
}

func (User) TableName() string { return "users" }

// IsAdmin: SUPER_ADMIN & ADMIN sama-sama memiliki akses penuh ke seluruh tim.
func (u User) IsAdmin() bool {
	return u.Role == RoleSuperAdmin || u.Role == RoleAdmin
}

type AppFolder struct {
	ID               string        `gorm:"primaryKey;type:nvarchar(64)" json:"id"`
	FolderName       string        `gorm:"type:nvarchar(200);not null" json:"folder_name"`
	Description      string        `gorm:"type:nvarchar(500)" json:"description"`
	AssignedTeamID   string        `gorm:"index;type:nvarchar(64);not null" json:"assigned_team_id"`
	AssignedBranchID *string       `gorm:"index;type:nvarchar(64)" json:"assigned_branch_id"`
	CreatedByBadge   string        `gorm:"type:nvarchar(50);not null" json:"created_by_badge"`
	CreatedByName    string        `gorm:"type:nvarchar(150);not null" json:"created_by_name"`
	CreatedAt        time.Time     `gorm:"autoCreateTime" json:"created_at"`
	AssignedTeam     *MasterTeam   `gorm:"foreignKey:AssignedTeamID" json:"assigned_team,omitempty"`
	AssignedBranch   *MasterBranch `gorm:"foreignKey:AssignedBranchID" json:"assigned_branch,omitempty"`
	Applications     []Application `gorm:"foreignKey:FolderID" json:"applications,omitempty"`
}

func (AppFolder) TableName() string { return "app_folders" }

type Application struct {
	ID             string             `gorm:"primaryKey;type:nvarchar(64)" json:"id"`
	FolderID       string             `gorm:"index;type:nvarchar(64);not null" json:"folder_id"`
	AppCode        string             `gorm:"index;type:nvarchar(50);not null" json:"app_code"`
	AppName        string             `gorm:"type:nvarchar(200);not null" json:"app_name"`
	Description    string             `gorm:"type:nvarchar(1000)" json:"description"`
	Status         string             `gorm:"type:nvarchar(50);not null;default:'PRODUCTION'" json:"status"`
	CreatedByBadge string             `gorm:"type:nvarchar(50);not null" json:"created_by_badge"`
	CreatedByName  string             `gorm:"type:nvarchar(150);not null" json:"created_by_name"`
	CreatedAt      time.Time          `gorm:"autoCreateTime" json:"created_at"`
	Folder         *AppFolder         `gorm:"foreignKey:FolderID" json:"folder,omitempty"`
	Sections       []AppChangeSection `gorm:"foreignKey:ApplicationID" json:"sections,omitempty"`
}

func (Application) TableName() string { return "applications" }

type AppChangeSection struct {
	ID                string     `gorm:"primaryKey;type:nvarchar(64)" json:"id"`
	ApplicationID     string     `gorm:"index;type:nvarchar(64);not null" json:"application_id"`
	VersionLabel      string     `gorm:"type:nvarchar(50);not null" json:"version_label"`
	SectionTitle      string     `gorm:"type:nvarchar(200);not null" json:"section_title"`
	ChangeDescription string     `gorm:"type:nvarchar(1000)" json:"change_description"`
	IsInitialRelease  bool       `gorm:"not null;default:false" json:"is_initial_release"`
	SortOrder         int        `gorm:"not null;default:1" json:"sort_order"`
	ReleaseDate       string     `gorm:"type:nvarchar(30)" json:"release_date"`
	CreatedByBadge    string     `gorm:"type:nvarchar(50);not null" json:"created_by_badge"`
	CreatedByName     string     `gorm:"type:nvarchar(150);not null" json:"created_by_name"`
	CreatedAt         time.Time  `gorm:"autoCreateTime" json:"created_at"`
	Documents         []Document `gorm:"foreignKey:SectionID" json:"documents,omitempty"`
}

func (AppChangeSection) TableName() string { return "app_change_sections" }

type Document struct {
	ID                string     `gorm:"primaryKey;type:nvarchar(64)" json:"id"`
	ModuleSource      string     `gorm:"index;type:nvarchar(30);not null" json:"module_source"` // APPLICATION | EVIDENCE
	FolderID          *string    `gorm:"index;type:nvarchar(64)" json:"folder_id"`
	ApplicationID     *string    `gorm:"index;type:nvarchar(64)" json:"application_id"`
	SectionID         *string    `gorm:"index;type:nvarchar(64)" json:"section_id"`
	DocCategory       string     `gorm:"type:nvarchar(50);not null" json:"doc_category"` // PRD, UAT, BAST, FSD, SOP, SLA, DRC, PENTEST, LAINNYA
	ITDomain          string     `gorm:"type:nvarchar(100)" json:"it_domain"`
	Title             string     `gorm:"type:nvarchar(255);not null" json:"title"`
	StandardizedName  string     `gorm:"type:nvarchar(255);not null" json:"standardized_name"`
	OriginalFilename  string     `gorm:"type:nvarchar(255);not null" json:"original_filename"`
	FileExtension     string     `gorm:"type:nvarchar(20);not null" json:"file_extension"`
	FilePath          string     `gorm:"type:nvarchar(500);not null" json:"file_path"`
	FileSizeBytes     int64      `gorm:"not null;default:0" json:"file_size_bytes"`
	FileHashSHA256    string     `gorm:"type:nvarchar(64)" json:"file_hash_sha256"`
	VersionLabel      string     `gorm:"type:nvarchar(50);not null;default:'v1.0'" json:"version_label"`
	AuditTags         string     `gorm:"type:nvarchar(500)" json:"audit_tags"`
	ValidUntil        string     `gorm:"type:nvarchar(30)" json:"valid_until"`
	ValidityStatus    string     `gorm:"type:nvarchar(30);not null;default:'VALID'" json:"validity_status"`
	VisibilityScope   string     `gorm:"index;type:nvarchar(30);not null;default:'TEAM_ONLY'" json:"visibility_scope"` // TEAM_ONLY | ALL_TIM
	OwnerTeamID       *string    `gorm:"index;type:nvarchar(64)" json:"owner_team_id"`
	OwnerBranchID     *string    `gorm:"index;type:nvarchar(64)" json:"owner_branch_id"`
	AISummary         string     `gorm:"type:nvarchar(max)" json:"ai_summary"`
	ExtractedText     string     `gorm:"type:nvarchar(max)" json:"extracted_text"` // cuplikan awal isi file; isi lengkap ada di document_chunks
	ContentIndexedAt  *time.Time `json:"content_indexed_at"`                       // nil = isi file belum dibaca untuk Agentic AI
	ContentChunkCount int        `gorm:"not null;default:0" json:"content_chunk_count"`
	// Versi pipeline indeks isi; dokumen di bawah versi terkini diindeks ulang otomatis saat start
	ContentIndexVersion    int               `gorm:"not null;default:0" json:"content_index_version"`
	ContentPageCount       *int              `json:"content_page_count"`                                                // jumlah halaman/slide/sheet; nil = tidak berhalaman
	ContentUnreadPageCount int               `gorm:"not null;default:0" json:"content_unread_page_count"`               // halaman bergambar tanpa teks (hasil scan)
	UploadChannel          string            `gorm:"type:nvarchar(20);not null;default:'MANUAL'" json:"upload_channel"` // MANUAL | AGENTIC_AI
	UploadedByBadge        string            `gorm:"type:nvarchar(50);not null" json:"uploaded_by_badge"`
	UploadedByName         string            `gorm:"type:nvarchar(150);not null" json:"uploaded_by_name"`
	UploadedByRole         string            `gorm:"type:nvarchar(30);not null" json:"uploaded_by_role"`
	UploadedByTeamLabel    string            `gorm:"type:nvarchar(200)" json:"uploaded_by_team_label"`
	UploadedAt             time.Time         `gorm:"autoCreateTime" json:"uploaded_at"`
	OwnerTeam              *MasterTeam       `gorm:"foreignKey:OwnerTeamID" json:"owner_team,omitempty"`
	OwnerBranch            *MasterBranch     `gorm:"foreignKey:OwnerBranchID" json:"owner_branch,omitempty"`
	Application            *Application      `gorm:"foreignKey:ApplicationID" json:"application,omitempty"`
	Folder                 *AppFolder        `gorm:"foreignKey:FolderID" json:"folder,omitempty"`
	Section                *AppChangeSection `gorm:"foreignKey:SectionID" json:"section,omitempty"`
}

func (Document) TableName() string { return "documents" }

type DocumentChunk struct {
	ID         string    `gorm:"primaryKey;type:nvarchar(64)" json:"id"`
	DocumentID string    `gorm:"index;type:nvarchar(64);not null" json:"document_id"`
	ChunkIndex int       `gorm:"not null" json:"chunk_index"`
	ChunkText  string    `gorm:"type:nvarchar(max);not null" json:"chunk_text"`
	TokenCount int       `gorm:"not null;default:0" json:"token_count"` // jumlah kata, untuk normalisasi panjang BM25
	CreatedAt  time.Time `gorm:"autoCreateTime" json:"created_at"`
}

func (DocumentChunk) TableName() string { return "document_chunks" }

// DocumentChunkTerm adalah indeks kata (inverted index) per chunk: kata dasar hasil stemming beserta frekuensinya.
type DocumentChunkTerm struct {
	ChunkID    string `gorm:"primaryKey;type:nvarchar(64)" json:"chunk_id"`
	Term       string `gorm:"primaryKey;type:nvarchar(64)" json:"term"`
	DocumentID string `gorm:"type:nvarchar(64);not null" json:"document_id"`
	TF         int    `gorm:"column:tf;not null" json:"tf"`
}

func (DocumentChunkTerm) TableName() string { return "document_chunk_terms" }

type UploadAuditLog struct {
	ID                 int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	BadgeNumber        string    `gorm:"index;type:nvarchar(50);not null" json:"badge_number"`
	UserName           string    `gorm:"type:nvarchar(150);not null" json:"user_name"`
	UserRole           string    `gorm:"type:nvarchar(30);not null" json:"user_role"`
	TeamAndBranchLabel string    `gorm:"type:nvarchar(200)" json:"team_and_branch_label"`
	ActionType         string    `gorm:"type:nvarchar(50);not null" json:"action_type"`
	ModuleSource       string    `gorm:"type:nvarchar(30)" json:"module_source"`
	DocumentID         *string   `gorm:"type:nvarchar(64)" json:"document_id"`
	FolderName         string    `gorm:"type:nvarchar(200)" json:"folder_name"`
	AppName            string    `gorm:"type:nvarchar(200)" json:"app_name"`
	SectionVersion     string    `gorm:"type:nvarchar(100)" json:"section_version"`
	DocumentTitle      string    `gorm:"type:nvarchar(255)" json:"document_title"`
	VisibilityLabel    string    `gorm:"type:nvarchar(150)" json:"visibility_label"`
	DetailDescription  string    `gorm:"type:nvarchar(1000);not null" json:"detail_description"`
	ActionTimestamp    time.Time `gorm:"autoCreateTime;index" json:"action_timestamp"`
}

func (UploadAuditLog) TableName() string { return "upload_audit_logs" }

// UploadStaging menampung file yang diunggah lewat Agentic AI sampai user mengonfirmasi tujuannya.
// File staging tidak terlihat di modul mana pun dan tidak diindeks AI; dihapus saat konfirmasi/batal/kedaluwarsa.
type UploadStaging struct {
	ID             string    `gorm:"primaryKey;type:nvarchar(64)" json:"id"`
	UserID         string    `gorm:"index;type:nvarchar(64);not null" json:"user_id"`
	BadgeNumber    string    `gorm:"type:nvarchar(50);not null" json:"badge_number"`
	FilePath       string    `gorm:"type:nvarchar(500);not null" json:"-"`
	OriginalName   string    `gorm:"type:nvarchar(255);not null" json:"original_name"`
	FileExtension  string    `gorm:"type:nvarchar(20);not null" json:"file_extension"`
	FileSizeBytes  int64     `gorm:"not null" json:"file_size_bytes"`
	FileHashSHA256 string    `gorm:"type:nvarchar(64);not null" json:"file_hash_sha256"`
	UserMessage    string    `gorm:"type:nvarchar(1000)" json:"user_message"`
	SuggestionJSON string    `gorm:"type:nvarchar(max)" json:"-"` // saran AI, dicatat di log sebagai pembanding pilihan user
	CreatedAt      time.Time `gorm:"autoCreateTime" json:"created_at"`
	ExpiresAt      time.Time `gorm:"not null" json:"expires_at"`
}

func (UploadStaging) TableName() string { return "upload_staging" }

// AIChatSession adalah satu percakapan Agentic AI milik seorang user (riwayat chat).
// Waktu disimpan dalam UTC dan diisi manual agar tidak terkena pergeseran zona waktu driver DATETIME2.
type AIChatSession struct {
	ID           string    `gorm:"primaryKey;type:nvarchar(64)" json:"id"`
	UserID       string    `gorm:"type:nvarchar(64);not null" json:"-"`
	Title        string    `gorm:"type:nvarchar(200);not null" json:"title"`
	MessageCount int       `gorm:"not null;default:0" json:"message_count"`
	CreatedAt    time.Time `gorm:"not null;autoCreateTime:false" json:"created_at"`
	UpdatedAt    time.Time `gorm:"not null;autoUpdateTime:false" json:"updated_at"`
}

func (AIChatSession) TableName() string { return "ai_chat_sessions" }

// AIChatMessage adalah satu tanya-jawab dalam percakapan, termasuk dokumen yang dikutip saat itu.
type AIChatMessage struct {
	ID                   int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	SessionID            string    `gorm:"type:nvarchar(64);not null" json:"session_id"`
	Prompt               string    `gorm:"type:nvarchar(max);not null" json:"prompt"`
	FilterModule         string    `gorm:"type:nvarchar(30)" json:"filter_module"`
	ReplyMarkdown        string    `gorm:"type:nvarchar(max)" json:"reply_markdown"`
	EngineMode           string    `gorm:"type:nvarchar(200)" json:"engine_mode"`
	ScopeLabel           string    `gorm:"type:nvarchar(300)" json:"scope_label"`
	MatchedDocumentsJSON string    `gorm:"column:matched_documents_json;type:nvarchar(max)" json:"-"`
	TraceJSON            string    `gorm:"column:trace_json;type:nvarchar(max)" json:"-"`             // langkah AI (pencarian, rerank), token & durasi
	ReplyToJSON          string    `gorm:"column:reply_to_json;type:nvarchar(max)" json:"-"`          // pesan yang dibalas / diteruskan user
	AnswerBasis          string    `gorm:"column:answer_basis;type:nvarchar(20)" json:"answer_basis"` // DOCUMENTS | GENERAL | MIXED
	CreatedAt            time.Time `gorm:"not null;autoCreateTime:false" json:"created_at"`
}

func (AIChatMessage) TableName() string { return "ai_chat_messages" }

// LLMUsageLog mencatat setiap permintaan ke LLM eksternal untuk memantau pemakaian kuota.
// Waktu dalam UTC dan diisi manual (lihat catatan zona waktu di AIChatSession).
type LLMUsageLog struct {
	ID               int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	Provider         string     `gorm:"type:nvarchar(30);not null" json:"provider"`
	Model            string     `gorm:"type:nvarchar(150);not null" json:"model"`
	KeyFingerprint   string     `gorm:"type:nvarchar(16);not null" json:"-"` // potongan hash API key, untuk membedakan project/key
	StatusCode       int        `gorm:"not null" json:"status_code"`         // 0 = tidak terhubung ke provider
	Success          bool       `gorm:"not null" json:"success"`
	PromptTokens     int        `gorm:"not null;default:0" json:"prompt_tokens"`
	CompletionTokens int        `gorm:"not null;default:0" json:"completion_tokens"`
	QuotaLimit       *int       `json:"quota_limit"` // batas permintaan harian yang disebut provider pada error 429
	RetryUntil       *time.Time `json:"retry_until"` // kuota pulih (dari error 429)
	ErrorMessage     string     `gorm:"type:nvarchar(500)" json:"error_message"`
	CreatedAt        time.Time  `gorm:"not null;autoCreateTime:false" json:"created_at"`
}

func (LLMUsageLog) TableName() string { return "llm_usage_logs" }

// AppSetting menyimpan konfigurasi runtime (mis. LLM) yang dapat diubah Super Admin tanpa restart.
// Nilai secret (API key) disimpan terenkripsi AES-GCM.
type AppSetting struct {
	SettingKey     string    `gorm:"primaryKey;type:nvarchar(100)" json:"setting_key"`
	SettingValue   string    `gorm:"type:nvarchar(max)" json:"-"`
	IsSecret       bool      `gorm:"not null;default:false" json:"is_secret"`
	UpdatedByBadge string    `gorm:"type:nvarchar(50)" json:"updated_by_badge"`
	UpdatedAt      time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

func (AppSetting) TableName() string { return "app_settings" }
