package handler

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"panda-backend/internal/models"
	"panda-backend/internal/service"
	"panda-backend/internal/storage"
)

type Handler struct {
	DB        *gorm.DB
	JWTSecret []byte
}

func NewHandler(db *gorm.DB) *Handler {
	return &Handler{
		DB:        db,
		JWTSecret: []byte(service.AppSecret()),
	}
}

// Header impersonate yang dikirim frontend saat Super Admin memilih "Lihat sebagai" segmen Tim/Branch
const (
	headerImpersonateTeam   = "X-Impersonate-Team"
	headerImpersonateBranch = "X-Impersonate-Branch"
)

// ─── Middleware Auth by JWT ──────────────────────────────────────────────────

func (h *Handler) AuthMiddleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		authHeader := c.Get("Authorization")
		tokenStr := ""
		if strings.HasPrefix(authHeader, "Bearer ") {
			tokenStr = strings.TrimPrefix(authHeader, "Bearer ")
		}

		if tokenStr == "" {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "Token autentikasi diperlukan"})
		}

		token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
			return h.JWTSecret, nil
		}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
		if err != nil || !token.Valid {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "Token tidak valid atau telah kedaluwarsa"})
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "Klaim token tidak valid"})
		}

		badge, _ := claims["badge_number"].(string)
		var user models.User
		if err := h.DB.Preload("Team").Preload("Branch").Where("badge_number = ? AND is_active = ?", badge, true).First(&user).Error; err != nil {
			return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "Pengguna tidak ditemukan atau tidak aktif"})
		}

		if teamID := strings.TrimSpace(c.Get(headerImpersonateTeam)); teamID != "" {
			effective, status, msg := h.buildImpersonatedUser(c, user, teamID, strings.TrimSpace(c.Get(headerImpersonateBranch)))
			if status != 0 {
				return c.Status(status).JSON(fiber.Map{"error": msg})
			}
			c.Locals("realUser", user)
			user = effective
		}

		c.Locals("currentUser", user)
		return c.Next()
	}
}

// buildImpersonatedUser membentuk identitas efektif Pegawai TI pada segmen Tim/Branch terpilih,
// sehingga seluruh filter isolasi data berjalan persis seperti pegawai di segmen tersebut.
// Mode ini read-only agar tidak ada data yang tercatat atas nama segmen palsu.
func (h *Handler) buildImpersonatedUser(c fiber.Ctx, real models.User, teamID, branchID string) (models.User, int, string) {
	if real.Role != models.RoleSuperAdmin {
		return real, http.StatusForbidden, "Hanya Super Admin yang dapat melakukan impersonate segmen"
	}

	readOnlyAllowed := c.Method() == http.MethodGet ||
		(c.Method() == http.MethodPost && (c.Path() == "/api/v1/ai/chat" || c.Path() == "/api/v1/ai/chat/stream"))
	if !readOnlyAllowed {
		return real, http.StatusForbidden, "Mode impersonate bersifat read-only. Keluar dari impersonate untuk mengubah data."
	}

	var team models.MasterTeam
	if err := h.DB.First(&team, "id = ?", teamID).Error; err != nil {
		return real, http.StatusBadRequest, "Tim impersonate tidak ditemukan"
	}

	effective := real
	effective.Role = models.RolePegawaiTI
	effective.TeamID = &team.ID
	effective.Team = &team
	effective.BranchID = nil
	effective.Branch = nil

	if branchID != "" {
		var branch models.MasterBranch
		if err := h.DB.First(&branch, "id = ? AND team_id = ?", branchID, team.ID).Error; err != nil {
			return real, http.StatusBadRequest, "Branch impersonate tidak ditemukan pada tim tersebut"
		}
		effective.BranchID = &branch.ID
		effective.Branch = &branch
	}
	return effective, 0, ""
}

func getCurrentUser(c fiber.Ctx) models.User {
	val := c.Locals("currentUser")
	if val == nil {
		return models.User{}
	}
	return val.(models.User)
}

// getRealUser mengembalikan akun yang benar-benar login (berbeda dari currentUser saat impersonate).
func getRealUser(c fiber.Ctx) (models.User, bool) {
	if val := c.Locals("realUser"); val != nil {
		return val.(models.User), true
	}
	return getCurrentUser(c), false
}

func formatUserTeamLabel(u models.User) string { return service.UserTeamLabel(u) }

// ─── Public Landing Page Stats ───────────────────────────────────────────────

func (h *Handler) GetPublicStats(c fiber.Ctx) error {
	var totalApps, totalAppDocs, totalEvidence, totalDocs int64
	h.DB.Model(&models.Application{}).Count(&totalApps)
	h.DB.Model(&models.Document{}).Where("module_source = ?", "APPLICATION").Count(&totalAppDocs)
	h.DB.Model(&models.Document{}).Where("module_source = ?", "EVIDENCE").Count(&totalEvidence)
	h.DB.Model(&models.Document{}).Count(&totalDocs)

	return c.Status(http.StatusOK).JSON(fiber.Map{
		"data": fiber.Map{
			"total_apps":       totalApps,
			"total_app_docs":   totalAppDocs,
			"total_evidence":   totalEvidence,
			"total_ai_indexed": totalDocs,
			"compliance_rate":  96,
		},
	})
}

// ─── Auth Login by Badge & Bypass Password "Panda" ───────────────────────────

type loginRequest struct {
	BadgeNumber string `json:"badge_number" binding:"required"`
	Password    string `json:"password" binding:"required"`
}

func (h *Handler) Login(c fiber.Ctx) error {
	var req loginRequest
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "Nomor Badge dan Password wajib diisi"})

	}

	badge := strings.TrimSpace(req.BadgeNumber)
	pass := strings.TrimSpace(req.Password)
	bypass := os.Getenv("BYPASS_PASSWORD")
	if bypass == "" {
		bypass = "Panda"
	}

	var user models.User
	if err := h.DB.Preload("Team").Preload("Branch").Where("badge_number = ?", badge).First(&user).Error; err != nil {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": fmt.Sprintf("Badge %s belum terdaftar di Master Akses PANDA", badge)})

	}

	if !user.IsActive {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "Akun Badge Anda sedang dinonaktifkan oleh Admin"})

	}

	// Verify bypass password "Panda" or user's stored password
	if !strings.EqualFold(pass, bypass) && pass != user.PasswordHash {
		return c.Status(http.StatusUnauthorized).JSON(fiber.Map{"error": "Password tidak sesuai (Gunakan password bypass: Panda)"})

	}

	claims := jwt.MapClaims{
		"user_id":      user.ID,
		"badge_number": user.BadgeNumber,
		"role":         user.Role,
		"exp":          time.Now().Add(72 * time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(h.JWTSecret)
	if err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Gagal membuat token sesi"})

	}

	return c.Status(http.StatusOK).JSON(fiber.Map{
		"token": signed,
		"user":  user,
	})
}

func (h *Handler) GetMe(c fiber.Ctx) error {
	user := getCurrentUser(c)
	real, impersonating := getRealUser(c)
	return c.Status(http.StatusOK).JSON(fiber.Map{
		"user":          user,
		"real_user":     real,
		"impersonating": impersonating,
	})
}

// ─── Dashboard Summary (Bento Grid Data) ─────────────────────────────────────

func (h *Handler) GetDashboardSummary(c fiber.Ctx) error {
	user := getCurrentUser(c)

	folderQuery := applyFolderScope(h.DB.Model(&models.AppFolder{}), user).Preload("AssignedTeam").Preload("AssignedBranch").Preload("Applications")
	docQuery := service.ApplyDocumentScope(h.DB.Model(&models.Document{}), user).Preload("Application").Preload("Folder").Preload("Section")
	logQuery := h.scopedUploadLogs(c)

	var folders []models.AppFolder
	folderQuery.Order("created_at DESC").Find(&folders)

	var docs []models.Document
	docQuery.Order("uploaded_at DESC").Find(&docs)

	var recentLogs []models.UploadAuditLog
	logQuery.Order("action_timestamp DESC").Limit(12).Find(&recentLogs)

	totalApps := 0
	for _, f := range folders {
		totalApps += len(f.Applications)
	}

	appDocsCount := 0
	evidenceDocsCount := 0
	for _, d := range docs {
		if d.ModuleSource == "APPLICATION" {
			appDocsCount++
		} else {
			evidenceDocsCount++
		}
	}

	// Team distribution stats for Donut Chart
	teamDist := map[string]int{
		"Tim Digitalisasi":     0,
		"Tim Layanan TI":       0,
		"Tim Infrastruktur TI": 0,
		"Tim Tata Kelola TI":   0,
	}
	for _, d := range docs {
		lbl := d.UploadedByTeamLabel
		switch {
		case strings.Contains(lbl, "Digitalisasi"):
			teamDist["Tim Digitalisasi"]++
		case strings.Contains(lbl, "Layanan"):
			teamDist["Tim Layanan TI"]++
		case strings.Contains(lbl, "Infrastruktur"):
			teamDist["Tim Infrastruktur TI"]++
		default:
			teamDist["Tim Tata Kelola TI"]++
		}
	}

	recentDocs := docs
	if len(recentDocs) > 6 {
		recentDocs = recentDocs[:6]
	}

	var totalUsers int64
	h.DB.Model(&models.User{}).Where("is_active = ?", true).Count(&totalUsers)

	return c.Status(http.StatusOK).JSON(fiber.Map{
		"summary": fiber.Map{
			"total_folders":       len(folders),
			"total_apps":          totalApps,
			"total_app_docs":      appDocsCount,
			"total_evidence_docs": evidenceDocsCount,
			"total_all_docs":      len(docs),
			"total_users":         totalUsers,
			"compliance_rate":     96,
			"user_scope_label":    formatUserTeamLabel(user),
		},
		"team_distribution": teamDist,
		"folders":           folders,
		"recent_documents":  recentDocs,
		"recent_logs":       recentLogs,
	})
}

// ─── Fitur Aplikasi: Folder Tim -> Aplikasi -> Section Perubahan ─────────────

func (h *Handler) GetFolders(c fiber.Ctx) error {
	user := getCurrentUser(c)
	query := h.DB.Model(&models.AppFolder{}).
		Preload("AssignedTeam").
		Preload("AssignedBranch").
		Preload("Applications").
		Preload("Applications.Sections").
		Preload("Applications.Sections.Documents")

	query = applyFolderScope(query, user)

	var folders []models.AppFolder
	query.Order("created_at DESC").Find(&folders)
	return c.Status(http.StatusOK).JSON(fiber.Map{"folders": folders})
}

type createFolderRequest struct {
	FolderName       string  `json:"folder_name" binding:"required"`
	Description      string  `json:"description"`
	AssignedTeamID   string  `json:"assigned_team_id" binding:"required"`
	AssignedBranchID *string `json:"assigned_branch_id"`
}

func (h *Handler) CreateFolder(c fiber.Ctx) error {
	user := getCurrentUser(c)
	if !user.IsAdmin() {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "Hanya Admin yang dapat membuat Folder dan meng-assign ke Tim"})

	}

	var req createFolderRequest
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "Nama Folder dan Tim tujuan wajib diisi"})

	}

	var branchID *string
	if req.AssignedBranchID != nil && strings.TrimSpace(*req.AssignedBranchID) != "" {
		branchID = req.AssignedBranchID
	}

	folder := models.AppFolder{
		ID:               uuid.NewString(),
		FolderName:       strings.TrimSpace(req.FolderName),
		Description:      strings.TrimSpace(req.Description),
		AssignedTeamID:   req.AssignedTeamID,
		AssignedBranchID: branchID,
		CreatedByBadge:   user.BadgeNumber,
		CreatedByName:    user.FullName,
	}

	if err := h.DB.Create(&folder).Error; err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Gagal membuat folder"})

	}

	h.DB.Preload("AssignedTeam").Preload("AssignedBranch").First(&folder, "id = ?", folder.ID)

	teamLabel := ""
	if folder.AssignedTeam != nil {
		teamLabel = folder.AssignedTeam.TeamName
	}
	if folder.AssignedBranch != nil {
		teamLabel += " • " + folder.AssignedBranch.BranchName
	}

	h.DB.Create(&models.UploadAuditLog{
		BadgeNumber:        user.BadgeNumber,
		UserName:           user.FullName,
		UserRole:           user.Role,
		TeamAndBranchLabel: formatUserTeamLabel(user),
		ActionType:         "CREATE_FOLDER",
		ModuleSource:       "APPLICATION",
		FolderName:         folder.FolderName,
		VisibilityLabel:    teamLabel,
		DetailDescription:  fmt.Sprintf("Admin %s (%s) membuat folder '%s' dan meng-assign ke %s", user.FullName, user.BadgeNumber, folder.FolderName, teamLabel),
		ActionTimestamp:    time.Now(),
	})

	return c.Status(http.StatusCreated).JSON(fiber.Map{"folder": folder})
}

type createAppRequest struct {
	AppCode        string `json:"app_code" binding:"required"`
	AppName        string `json:"app_name" binding:"required"`
	Description    string `json:"description"`
	Status         string `json:"status"`
	InitialVersion string `json:"initial_version"`
	InitialTitle   string `json:"initial_title"`
}

func (h *Handler) CreateApplication(c fiber.Ctx) error {
	user := getCurrentUser(c)
	if !user.IsAdmin() {
		return c.Status(http.StatusForbidden).JSON(fiber.Map{"error": "Hanya Admin yang dapat menambahkan Aplikasi ke dalam Folder Tim"})

	}

	folderID := c.Params("id")
	var folder models.AppFolder
	if err := h.DB.WithContext(c.Context()).First(&folder, "id = ?", folderID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "Folder tidak ditemukan"})
		} else {
			log.Printf("[Application] gagal memuat folder: %v", err)
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Gagal membaca folder"})
		}

	}

	var req createAppRequest
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "Kode dan Nama Aplikasi wajib diisi"})

	}

	initVer := strings.TrimSpace(req.InitialVersion)
	if initVer == "" {
		initVer = "v1.0"
	}
	if !validVersionLabel(initVer) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "Label versi maksimal 50 karakter dan tidak boleh mengandung pemisah path atau karakter khusus nama file"})

	}

	status := req.Status
	if status == "" {
		status = "PRODUCTION"
	}

	app := models.Application{
		ID:             uuid.NewString(),
		FolderID:       folder.ID,
		AppCode:        strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(req.AppCode), " ", "_")),
		AppName:        strings.TrimSpace(req.AppName),
		Description:    strings.TrimSpace(req.Description),
		Status:         status,
		CreatedByBadge: user.BadgeNumber,
		CreatedByName:  user.FullName,
	}

	initTitle := strings.TrimSpace(req.InitialTitle)
	if initTitle == "" {
		initTitle = "Pembuatan Awal Aplikasi (Initial Release)"
	}

	// Otomatis buat Section Pertama (v1.0 - Pembuatan Awal)
	initSection := models.AppChangeSection{
		ID:                uuid.NewString(),
		ApplicationID:     app.ID,
		VersionLabel:      initVer,
		SectionTitle:      initTitle,
		ChangeDescription: "Section awal saat pembuatan/registrasi aplikasi pertama kali.",
		IsInitialRelease:  true,
		SortOrder:         1,
		ReleaseDate:       time.Now().Format("2006-01-02"),
		CreatedByBadge:    user.BadgeNumber,
		CreatedByName:     user.FullName,
	}

	audit := models.UploadAuditLog{
		BadgeNumber:        user.BadgeNumber,
		UserName:           user.FullName,
		UserRole:           user.Role,
		TeamAndBranchLabel: formatUserTeamLabel(user),
		ActionType:         "CREATE_APPLICATION",
		ModuleSource:       "APPLICATION",
		FolderName:         folder.FolderName,
		AppName:            app.AppName,
		SectionVersion:     initSection.VersionLabel,
		DetailDescription:  fmt.Sprintf("Admin %s (%s) mendaftarkan aplikasi '%s' (%s) di dalam folder '%s'", user.FullName, user.BadgeNumber, app.AppName, app.AppCode, folder.FolderName),
		ActionTimestamp:    time.Now(),
	}

	created, err := service.CreateApplication(c.Context(), h.DB, app, initSection, audit)
	if err != nil {
		if errors.Is(err, service.ErrApplicationCodeUsed) {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "Kode Aplikasi sudah digunakan"})
		} else {
			log.Printf("[Application] gagal membuat aplikasi: %v", err)
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Gagal membuat aplikasi"})
		}

	}
	return c.Status(http.StatusCreated).JSON(fiber.Map{"application": created})
}

func (h *Handler) GetApplicationDetail(c fiber.Ctx) error {
	user := getCurrentUser(c)
	appID := c.Params("id")
	var app models.Application
	err := h.DB.
		Preload("Folder").
		Preload("Folder.AssignedTeam").
		Preload("Folder.AssignedBranch").
		Preload("Sections", func(db *gorm.DB) *gorm.DB {
			return db.Order("sort_order ASC, created_at ASC")
		}).
		Preload("Sections.Documents", func(db *gorm.DB) *gorm.DB {
			return db.Order("uploaded_at DESC")
		}).
		First(&app, "id = ?", appID).Error

	if err != nil || app.Folder == nil || !canAccessFolder(user, *app.Folder) {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "Aplikasi tidak ditemukan"})

	}
	return c.Status(http.StatusOK).JSON(fiber.Map{"application": app})
}

type createSectionRequest struct {
	VersionLabel      string `json:"version_label" binding:"required"`
	SectionTitle      string `json:"section_title" binding:"required"`
	ChangeDescription string `json:"change_description"`
	ReleaseDate       string `json:"release_date"`
}

func (h *Handler) CreateAppSection(c fiber.Ctx) error {
	user := getCurrentUser(c)
	appID := c.Params("id")

	var app models.Application
	if err := h.DB.Preload("Folder").First(&app, "id = ?", appID).Error; err != nil || app.Folder == nil || !canAccessFolder(user, *app.Folder) {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "Aplikasi tidak ditemukan"})

	}

	var req createSectionRequest
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "Label Versi dan Nama Section Perubahan wajib diisi"})

	}
	req.VersionLabel = strings.TrimSpace(req.VersionLabel)
	if !validVersionLabel(req.VersionLabel) {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "Label versi maksimal 50 karakter dan tidak boleh mengandung pemisah path atau karakter khusus nama file"})

	}

	var maxOrder int64
	h.DB.Model(&models.AppChangeSection{}).Where("application_id = ?", app.ID).Count(&maxOrder)

	relDate := req.ReleaseDate
	if relDate == "" {
		relDate = time.Now().Format("2006-01-02")
	}

	section := models.AppChangeSection{
		ID:                uuid.NewString(),
		ApplicationID:     app.ID,
		VersionLabel:      strings.TrimSpace(req.VersionLabel),
		SectionTitle:      strings.TrimSpace(req.SectionTitle),
		ChangeDescription: strings.TrimSpace(req.ChangeDescription),
		IsInitialRelease:  false,
		SortOrder:         int(maxOrder) + 1,
		ReleaseDate:       relDate,
		CreatedByBadge:    user.BadgeNumber,
		CreatedByName:     user.FullName,
	}

	if err := h.DB.Create(&section).Error; err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Gagal membuat section perubahan"})

	}

	folderName := ""
	if app.Folder != nil {
		folderName = app.Folder.FolderName
	}

	h.DB.Create(&models.UploadAuditLog{
		BadgeNumber:        user.BadgeNumber,
		UserName:           user.FullName,
		UserRole:           user.Role,
		TeamAndBranchLabel: formatUserTeamLabel(user),
		ActionType:         "CREATE_SECTION",
		ModuleSource:       "APPLICATION",
		FolderName:         folderName,
		AppName:            app.AppName,
		SectionVersion:     fmt.Sprintf("%s - %s", section.VersionLabel, section.SectionTitle),
		DetailDescription:  fmt.Sprintf("%s (%s) menambahkan section perubahan [%s - %s] pada aplikasi %s tanpa menghapus dokumen awal", user.FullName, user.BadgeNumber, section.VersionLabel, section.SectionTitle, app.AppName),
		ActionTimestamp:    time.Now(),
	})

	return c.Status(http.StatusCreated).JSON(fiber.Map{"section": section})
}

// ─── Fitur Menu "Evidence" & Upload Dokumen (dengan Log Siapa & Kapan) ───────

func (h *Handler) GetEvidenceList(c fiber.Ctx) error {
	user := getCurrentUser(c)
	query := h.DB.Model(&models.Document{}).
		Preload("OwnerTeam").
		Preload("OwnerBranch").
		Preload("Application").
		Preload("Folder").
		Preload("Section")

	moduleFilter := c.Query("module_source")
	if moduleFilter != "" {
		query = query.Where("module_source = ?", moduleFilter)
	}

	query = service.ApplyDocumentScope(query, user)

	var docs []models.Document
	query.Order("uploaded_at DESC").Find(&docs)
	return c.Status(http.StatusOK).JSON(fiber.Map{"documents": docs})
}

func (h *Handler) UploadDocument(c fiber.Ctx) error {
	user := getCurrentUser(c)
	if status, msg := parseUploadForm(c); status != 0 {
		return c.Status(status).JSON(fiber.Map{"error": msg})

	}

	moduleSource := strings.ToUpper(strings.TrimSpace(c.FormValue("module_source")))
	if moduleSource == "" {
		moduleSource = "EVIDENCE"
	}
	target, status, msg := h.resolveUploadTarget(user, uploadTarget{
		ModuleSource:    moduleSource,
		ApplicationID:   c.FormValue("application_id"),
		SectionID:       c.FormValue("section_id"),
		VisibilityScope: c.FormValue("visibility_scope"),
		TargetTeamID:    c.FormValue("target_team_id"),
		TargetBranchID:  c.FormValue("target_branch_id"),
	})
	if status != 0 {
		return c.Status(status).JSON(fiber.Map{"error": msg})

	}

	file, status, msg := readUploadedFile(c)
	if status != 0 {
		return c.Status(status).JSON(fiber.Map{"error": msg})

	}

	meta := documentMeta{
		Category:      c.FormValue("doc_category"),
		Title:         c.FormValue("title"),
		ITDomain:      c.FormValue("it_domain"),
		VersionLabel:  c.FormValue("version_label"),
		AuditTags:     c.FormValue("audit_tags"),
		ValidUntil:    c.FormValue("valid_until"),
		Summary:       c.FormValue("ai_summary"),
		UploadChannel: UploadChannelManual,
	}
	if status, msg := normalizeDocumentMeta(&meta, target, user); status != 0 {
		return c.Status(status).JSON(fiber.Map{"error": msg})

	}

	doc, logEntry, contentIndex, err := h.persistDocument(user, target, meta, file)
	if err != nil {
		log.Printf("[Upload] %v", err)
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Gagal menyimpan dokumen. Silakan coba lagi."})

	}

	return c.Status(http.StatusCreated).JSON(fiber.Map{
		"document":      doc,
		"log":           logEntry,
		"content_index": contentIndex,
	})
}

// serveDocumentFile mengirim file fisik dokumen. File yang hilang dilaporkan sebagai 404,
// bukan diganti PDF buatan, agar auditor tidak pernah melihat dokumen yang bukan file asli.
func (h *Handler) serveDocumentFile(c fiber.Ctx, disposition string) error {
	var doc models.Document
	if err := service.ApplyDocumentScope(h.DB.Model(&models.Document{}), getCurrentUser(c)).Where("id = ?", c.Params("id")).First(&doc).Error; err != nil {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "Dokumen tidak ditemukan"})

	}
	// File dibaca dari tempat penyimpanannya (folder lokal atau MinIO, sesuai lokasi yang tercatat)
	file, info, err := storage.Open(c.Context(), doc.FilePath)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			log.Printf("[Storage] file fisik dokumen %s tidak ditemukan: %s", doc.ID, doc.FilePath)
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "File fisik dokumen tidak ditemukan di storage. Hubungi Admin."})

		}
		log.Printf("[Storage] file dokumen %s tidak dapat dibaca (%s): %v", doc.ID, doc.FilePath, err)
		return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "Storage dokumen sedang tidak dapat diakses. Coba lagi atau hubungi Admin."})

	}
	return sendDocument(c, doc.StandardizedName, disposition, file, info)
}

func (h *Handler) PreviewDocument(c fiber.Ctx) error {
	return h.serveDocumentFile(c, "inline")
}

func (h *Handler) DownloadDocument(c fiber.Ctx) error {
	return h.serveDocumentFile(c, "attachment")
}

// ─── Detail Log Aktivitas & Upload ───────────────────────────────────────────

// scopedUploadLogs memakai ID dokumen, bukan label tim yang bisa berubah atau cocok sebagian.
func (h *Handler) scopedUploadLogs(c fiber.Ctx) *gorm.DB {
	user := getCurrentUser(c)
	query := h.DB.Model(&models.UploadAuditLog{})
	if user.IsAdmin() {
		return query
	}
	docs := service.ApplyDocumentScope(h.DB.Model(&models.Document{}), user).Select("id")
	if _, impersonating := getRealUser(c); impersonating {
		return query.Where("action_type <> ? AND document_id IN (?)", "AI_SEARCH", docs)
	}
	return query.Where("badge_number = ? OR (action_type <> ? AND document_id IN (?))", user.BadgeNumber, "AI_SEARCH", docs)
}

func (h *Handler) GetUploadLogs(c fiber.Ctx) error {
	query := h.scopedUploadLogs(c)

	var logs []models.UploadAuditLog
	query.Order("action_timestamp DESC").Limit(100).Find(&logs)
	return c.Status(http.StatusOK).JSON(fiber.Map{"logs": logs})
}
