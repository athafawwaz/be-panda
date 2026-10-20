package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
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

func (h *Handler) AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		tokenStr := ""
		if strings.HasPrefix(authHeader, "Bearer ") {
			tokenStr = strings.TrimPrefix(authHeader, "Bearer ")
		} else if qToken := c.Query("token"); qToken != "" {
			tokenStr = qToken
		}

		if tokenStr == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Token autentikasi diperlukan"})
			c.Abort()
			return
		}

		token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
			return h.JWTSecret, nil
		})
		if err != nil || !token.Valid {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Token tidak valid atau telah kedaluwarsa"})
			c.Abort()
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Klaim token tidak valid"})
			c.Abort()
			return
		}

		badge, _ := claims["badge_number"].(string)
		var user models.User
		if err := h.DB.Preload("Team").Preload("Branch").Where("badge_number = ? AND is_active = 1", badge).First(&user).Error; err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Pengguna tidak ditemukan atau tidak aktif"})
			c.Abort()
			return
		}

		if teamID := strings.TrimSpace(c.GetHeader(headerImpersonateTeam)); teamID != "" {
			effective, status, msg := h.buildImpersonatedUser(c, user, teamID, strings.TrimSpace(c.GetHeader(headerImpersonateBranch)))
			if status != 0 {
				c.JSON(status, gin.H{"error": msg})
				c.Abort()
				return
			}
			c.Set("realUser", user)
			user = effective
		}

		c.Set("currentUser", user)
		c.Next()
	}
}

// buildImpersonatedUser membentuk identitas efektif Pegawai TI pada segmen Tim/Branch terpilih,
// sehingga seluruh filter isolasi data berjalan persis seperti pegawai di segmen tersebut.
// Mode ini read-only agar tidak ada data yang tercatat atas nama segmen palsu.
func (h *Handler) buildImpersonatedUser(c *gin.Context, real models.User, teamID, branchID string) (models.User, int, string) {
	if real.Role != models.RoleSuperAdmin {
		return real, http.StatusForbidden, "Hanya Super Admin yang dapat melakukan impersonate segmen"
	}

	readOnlyAllowed := c.Request.Method == http.MethodGet ||
		(c.Request.Method == http.MethodPost && c.FullPath() == "/api/v1/ai/chat")
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

func getCurrentUser(c *gin.Context) models.User {
	val, exists := c.Get("currentUser")
	if !exists {
		return models.User{}
	}
	return val.(models.User)
}

// getRealUser mengembalikan akun yang benar-benar login (berbeda dari currentUser saat impersonate).
func getRealUser(c *gin.Context) (models.User, bool) {
	if val, exists := c.Get("realUser"); exists {
		return val.(models.User), true
	}
	return getCurrentUser(c), false
}

func formatUserTeamLabel(u models.User) string {
	switch u.Role {
	case models.RoleSuperAdmin:
		return "Super Admin (Semua Tim)"
	case models.RoleAdmin:
		return "Administrator (Semua Tim)"
	}
	if u.Team != nil && u.Branch != nil {
		return fmt.Sprintf("%s • %s", u.Team.TeamName, u.Branch.BranchName)
	}
	if u.Team != nil {
		return u.Team.TeamName
	}
	return "Pegawai TI"
}

// ─── Public Landing Page Stats ───────────────────────────────────────────────

func (h *Handler) GetPublicStats(c *gin.Context) {
	var totalApps, totalAppDocs, totalEvidence, totalDocs int64
	h.DB.Model(&models.Application{}).Count(&totalApps)
	h.DB.Model(&models.Document{}).Where("module_source = ?", "APPLICATION").Count(&totalAppDocs)
	h.DB.Model(&models.Document{}).Where("module_source = ?", "EVIDENCE").Count(&totalEvidence)
	h.DB.Model(&models.Document{}).Count(&totalDocs)

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
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

func (h *Handler) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Nomor Badge dan Password wajib diisi"})
		return
	}

	badge := strings.TrimSpace(req.BadgeNumber)
	pass := strings.TrimSpace(req.Password)
	bypass := os.Getenv("BYPASS_PASSWORD")
	if bypass == "" {
		bypass = "Panda"
	}

	var user models.User
	if err := h.DB.Preload("Team").Preload("Branch").Where("badge_number = ?", badge).First(&user).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": fmt.Sprintf("Badge %s belum terdaftar di Master Akses PANDA", badge)})
		return
	}

	if !user.IsActive {
		c.JSON(http.StatusForbidden, gin.H{"error": "Akun Badge Anda sedang dinonaktifkan oleh Admin"})
		return
	}

	// Verify bypass password "Panda" or user's stored password
	if !strings.EqualFold(pass, bypass) && pass != user.PasswordHash {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Password tidak sesuai (Gunakan password bypass: Panda)"})
		return
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal membuat token sesi"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token": signed,
		"user":  user,
	})
}

func (h *Handler) GetMe(c *gin.Context) {
	user := getCurrentUser(c)
	real, impersonating := getRealUser(c)
	c.JSON(http.StatusOK, gin.H{
		"user":          user,
		"real_user":     real,
		"impersonating": impersonating,
	})
}

// ─── Dashboard Summary (Bento Grid Data) ─────────────────────────────────────

func (h *Handler) GetDashboardSummary(c *gin.Context) {
	user := getCurrentUser(c)

	folderQuery := h.DB.Model(&models.AppFolder{}).Preload("AssignedTeam").Preload("AssignedBranch").Preload("Applications")
	docQuery := h.DB.Model(&models.Document{}).Preload("Application").Preload("Folder").Preload("Section")
	logQuery := h.DB.Model(&models.UploadAuditLog{})

	if !user.IsAdmin() {
		if user.TeamID != nil && user.BranchID != nil {
			folderQuery = folderQuery.Where("assigned_team_id = ? AND (assigned_branch_id IS NULL OR assigned_branch_id = '' OR assigned_branch_id = ?)", *user.TeamID, *user.BranchID)
			docQuery = docQuery.Where("(visibility_scope = 'ALL_TIM') OR (owner_team_id = ? AND (owner_branch_id IS NULL OR owner_branch_id = '' OR owner_branch_id = ?))", *user.TeamID, *user.BranchID)
		} else if user.TeamID != nil {
			folderQuery = folderQuery.Where("assigned_team_id = ?", *user.TeamID)
			docQuery = docQuery.Where("(visibility_scope = 'ALL_TIM') OR (owner_team_id = ?)", *user.TeamID)
		}
	}

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
	h.DB.Model(&models.User{}).Where("is_active = 1").Count(&totalUsers)

	c.JSON(http.StatusOK, gin.H{
		"summary": gin.H{
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

func (h *Handler) GetFolders(c *gin.Context) {
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
	c.JSON(http.StatusOK, gin.H{"folders": folders})
}

type createFolderRequest struct {
	FolderName       string  `json:"folder_name" binding:"required"`
	Description      string  `json:"description"`
	AssignedTeamID   string  `json:"assigned_team_id" binding:"required"`
	AssignedBranchID *string `json:"assigned_branch_id"`
}

func (h *Handler) CreateFolder(c *gin.Context) {
	user := getCurrentUser(c)
	if !user.IsAdmin() {
		c.JSON(http.StatusForbidden, gin.H{"error": "Hanya Admin yang dapat membuat Folder dan meng-assign ke Tim"})
		return
	}

	var req createFolderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Nama Folder dan Tim tujuan wajib diisi"})
		return
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal membuat folder"})
		return
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

	c.JSON(http.StatusCreated, gin.H{"folder": folder})
}

type createAppRequest struct {
	AppCode        string `json:"app_code" binding:"required"`
	AppName        string `json:"app_name" binding:"required"`
	Description    string `json:"description"`
	Status         string `json:"status"`
	InitialVersion string `json:"initial_version"`
	InitialTitle   string `json:"initial_title"`
}

func (h *Handler) CreateApplication(c *gin.Context) {
	user := getCurrentUser(c)
	if !user.IsAdmin() {
		c.JSON(http.StatusForbidden, gin.H{"error": "Hanya Admin yang dapat menambahkan Aplikasi ke dalam Folder Tim"})
		return
	}

	folderID := c.Param("id")
	var folder models.AppFolder
	if err := h.DB.Preload("AssignedTeam").Preload("AssignedBranch").First(&folder, "id = ?", folderID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Folder tidak ditemukan"})
		return
	}

	var req createAppRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Kode dan Nama Aplikasi wajib diisi"})
		return
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

	if err := h.DB.Create(&app).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Kode Aplikasi sudah digunakan atau gagal disimpan"})
		return
	}

	initVer := strings.TrimSpace(req.InitialVersion)
	if initVer == "" {
		initVer = "v1.0"
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
	h.DB.Create(&initSection)

	h.DB.Create(&models.UploadAuditLog{
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
	})

	h.DB.Preload("Sections").Preload("Sections.Documents").First(&app, "id = ?", app.ID)
	c.JSON(http.StatusCreated, gin.H{"application": app})
}

func (h *Handler) GetApplicationDetail(c *gin.Context) {
	appID := c.Param("id")
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

	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Aplikasi tidak ditemukan"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"application": app})
}

type createSectionRequest struct {
	VersionLabel      string `json:"version_label" binding:"required"`
	SectionTitle      string `json:"section_title" binding:"required"`
	ChangeDescription string `json:"change_description"`
	ReleaseDate       string `json:"release_date"`
}

func (h *Handler) CreateAppSection(c *gin.Context) {
	user := getCurrentUser(c)
	appID := c.Param("id")

	var app models.Application
	if err := h.DB.Preload("Folder").First(&app, "id = ?", appID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Aplikasi tidak ditemukan"})
		return
	}

	var req createSectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Label Versi dan Nama Section Perubahan wajib diisi"})
		return
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal membuat section perubahan"})
		return
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

	c.JSON(http.StatusCreated, gin.H{"section": section})
}

// ─── Fitur Menu "Evidence" & Upload Dokumen (dengan Log Siapa & Kapan) ───────

func (h *Handler) GetEvidenceList(c *gin.Context) {
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

	if !user.IsAdmin() {
		if user.TeamID != nil && user.BranchID != nil {
			query = query.Where("(visibility_scope = 'ALL_TIM') OR (owner_team_id = ? AND (owner_branch_id IS NULL OR owner_branch_id = '' OR owner_branch_id = ?))", *user.TeamID, *user.BranchID)
		} else if user.TeamID != nil {
			query = query.Where("(visibility_scope = 'ALL_TIM') OR (owner_team_id = ?)", *user.TeamID)
		} else {
			query = query.Where("visibility_scope = 'ALL_TIM'")
		}
	}

	var docs []models.Document
	query.Order("uploaded_at DESC").Find(&docs)
	c.JSON(http.StatusOK, gin.H{"documents": docs})
}

func (h *Handler) UploadDocument(c *gin.Context) {
	user := getCurrentUser(c)
	if status, msg := parseUploadForm(c); status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	moduleSource := strings.ToUpper(strings.TrimSpace(c.PostForm("module_source")))
	if moduleSource == "" {
		moduleSource = "EVIDENCE"
	}
	target, status, msg := h.resolveUploadTarget(user, uploadTarget{
		ModuleSource:    moduleSource,
		ApplicationID:   c.PostForm("application_id"),
		SectionID:       c.PostForm("section_id"),
		VisibilityScope: c.PostForm("visibility_scope"),
		TargetTeamID:    c.PostForm("target_team_id"),
		TargetBranchID:  c.PostForm("target_branch_id"),
	})
	if status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	file, status, msg := readUploadedFile(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	meta := documentMeta{
		Category:      c.PostForm("doc_category"),
		Title:         c.PostForm("title"),
		ITDomain:      c.PostForm("it_domain"),
		VersionLabel:  c.PostForm("version_label"),
		AuditTags:     c.PostForm("audit_tags"),
		ValidUntil:    c.PostForm("valid_until"),
		Summary:       c.PostForm("ai_summary"),
		UploadChannel: UploadChannelManual,
	}
	if status, msg := normalizeDocumentMeta(&meta, target, user); status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	doc, logEntry, contentIndex, err := h.persistDocument(user, target, meta, file)
	if err != nil {
		log.Printf("[Upload] %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menyimpan dokumen. Silakan coba lagi."})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"document":      doc,
		"log":           logEntry,
		"content_index": contentIndex,
	})
}

// serveDocumentFile mengirim file fisik dokumen. File yang hilang dilaporkan sebagai 404,
// bukan diganti PDF buatan, agar auditor tidak pernah melihat dokumen yang bukan file asli.
func (h *Handler) serveDocumentFile(c *gin.Context, disposition string) {
	var doc models.Document
	if err := h.DB.First(&doc, "id = ?", c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Dokumen tidak ditemukan"})
		return
	}
	// File dibaca dari tempat penyimpanannya (folder lokal atau MinIO, sesuai lokasi yang tercatat)
	file, info, err := storage.Open(c.Request.Context(), doc.FilePath)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			log.Printf("[Storage] file fisik dokumen %s tidak ditemukan: %s", doc.ID, doc.FilePath)
			c.JSON(http.StatusNotFound, gin.H{"error": "File fisik dokumen tidak ditemukan di storage. Hubungi Admin."})
			return
		}
		log.Printf("[Storage] file dokumen %s tidak dapat dibaca (%s): %v", doc.ID, doc.FilePath, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Storage dokumen sedang tidak dapat diakses. Coba lagi atau hubungi Admin."})
		return
	}
	defer file.Close()

	c.Header("Content-Disposition", fmt.Sprintf("%s; filename=\"%s\"", disposition, doc.StandardizedName))
	if disposition == "inline" && strings.HasSuffix(strings.ToLower(doc.StandardizedName), ".pdf") {
		c.Header("Content-Type", "application/pdf")
	}
	// ServeContent melayani HTTP Range sehingga pratinjau PDF tetap bisa dibuka per halaman
	http.ServeContent(c.Writer, c.Request, doc.StandardizedName, info.ModTime, file)
}

func (h *Handler) PreviewDocument(c *gin.Context) {
	h.serveDocumentFile(c, "inline")
}

func (h *Handler) DownloadDocument(c *gin.Context) {
	h.serveDocumentFile(c, "attachment")
}

// ─── Detail Log Aktivitas & Upload ───────────────────────────────────────────

func (h *Handler) GetUploadLogs(c *gin.Context) {
	user := getCurrentUser(c)
	query := h.DB.Model(&models.UploadAuditLog{})

	if !user.IsAdmin() {
		teamLabel := formatUserTeamLabel(user)
		if _, impersonating := getRealUser(c); impersonating {
			// Segmen impersonate tidak punya log "milik sendiri"; jangan ikutkan log pribadi Super Admin
			query = query.Where("team_and_branch_label LIKE ? OR visibility_label LIKE ?",
				"%"+teamLabel+"%", "%Semua Tim%")
		} else {
			query = query.Where("badge_number = ? OR team_and_branch_label LIKE ? OR visibility_label LIKE ?",
				user.BadgeNumber, "%"+teamLabel+"%", "%Semua Tim%")
		}
	}

	var logs []models.UploadAuditLog
	query.Order("action_timestamp DESC").Limit(100).Find(&logs)
	c.JSON(http.StatusOK, gin.H{"logs": logs})
}

// ─── Agentic AI Chat Endpoint (Team-Scoped RAG) ──────────────────────────────

func (h *Handler) AgenticAIChat(c *gin.Context) {
	user := getCurrentUser(c)
	var req service.AIChatRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Prompt) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Pertanyaan / Prompt tidak boleh kosong"})
		return
	}

	// Riwayat percakapan dibaca dari sesi milik akun asli (pemilik riwayat chat); jumlahnya diatur di Docs → API Key
	actor, impersonating := getRealUser(c)

	resp, err := service.ProcessAgenticQuery(h.DB, user, req, actor.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal memproses pencarian Agentic AI"})
		return
	}
	h.recordAgenticChat(user, actor, impersonating, req, resp)
	c.JSON(http.StatusOK, resp)
}

// recordAgenticChat mencatat pencarian Agentic AI di log audit dan menyimpannya ke riwayat chat milik user.
func (h *Handler) recordAgenticChat(user, actor models.User, impersonating bool, req service.AIChatRequest, resp *service.AIChatResponse) {
	// Saat impersonate, log dicatat atas identitas asli Super Admin agar tidak tercampur ke log segmen
	mode := ""
	if req.Thinking && resp.Trace != nil {
		mode = fmt.Sprintf(" [Thinking: %d pencarian, %d token]", resp.Trace.Searches, resp.Trace.Usage.TotalTokens)
	}
	detail := fmt.Sprintf("%s (Badge %s) mencari di Agentic AI: \"%s\" (%d dokumen ditemukan)%s", actor.FullName, actor.BadgeNumber, req.Prompt, len(resp.MatchedDocuments), mode)
	if impersonating {
		detail += fmt.Sprintf(" [impersonate sebagai %s]", formatUserTeamLabel(user))
	}

	h.DB.Create(&models.UploadAuditLog{
		BadgeNumber:        actor.BadgeNumber,
		UserName:           actor.FullName,
		UserRole:           actor.Role,
		TeamAndBranchLabel: formatUserTeamLabel(actor),
		ActionType:         "AI_SEARCH",
		ModuleSource:       "AGENTIC_AI",
		VisibilityLabel:    resp.ScopeLabel,
		DetailDescription:  truncateUTF8(detail, 1000),
		ActionTimestamp:    time.Now(),
	})

	// Simpan ke riwayat chat milik user. Saat impersonate (mode pratinjau read-only) tidak disimpan
	// agar riwayat Super Admin tidak tercampur dengan hasil pencarian segmen lain.
	if !impersonating {
		if sessionID, messageID, err := h.saveChatTurn(actor, req.SessionID, req, resp); err != nil {
			log.Printf("[Agentic AI] gagal menyimpan riwayat chat: %v", err)
		} else {
			resp.SessionID, resp.MessageID = sessionID, messageID
		}
	}
}

// AgenticAIChatStream menjawab pertanyaan Agentic AI sambil mengirim progres lewat Server-Sent Events:
// "step" (langkah AI: menimbang, mencari, membaca, menilai ulang, menyusun jawaban), "usage" (token),
// "answer_delta"/"answer_reset" (jawaban mengalir), lalu "done" (jawaban lengkap) atau "error".
// Menutup koneksi (tombol Stop) membatalkan pencarian & panggilan LLM yang sedang berjalan.
func (h *Handler) AgenticAIChatStream(c *gin.Context) {
	user := getCurrentUser(c)
	var req service.AIChatRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Prompt) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Pertanyaan / Prompt tidak boleh kosong"})
		return
	}
	actor, impersonating := getRealUser(c)

	c.Header("Content-Type", "text/event-stream")
	// no-transform: proxy/kompresor (mis. gzip pada rewrite Next.js) tidak boleh menampung stream; tanpa ini
	// langkah AI & jawaban baru tiba sekaligus di akhir
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	// Semua penulisan ke response dijaga mutex; setelah handler selesai (closed) tidak ada lagi yang menulis
	var mu sync.Mutex
	closed := false
	emit := func(event string, data any) {
		payload, err := json.Marshal(data)
		if err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, payload)
		c.Writer.Flush()
	}
	defer func() {
		mu.Lock()
		closed = true
		mu.Unlock()
	}()

	// Detak berkala agar koneksi tidak diputus proxy saat menunggu LLM
	ctx := c.Request.Context()
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				mu.Lock()
				if closed {
					mu.Unlock()
					return
				}
				fmt.Fprint(c.Writer, ": ping\n\n")
				c.Writer.Flush()
				mu.Unlock()
			case <-ctx.Done():
				return
			}
		}
	}()

	resp, err := service.AnswerAgenticQuery(ctx, h.DB, user, req, actor.ID, emit)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[Agentic AI] stream gagal: %v", err)
			emit("error", gin.H{"error": "Gagal memproses pencarian Agentic AI"})
		}
		return
	}
	h.recordAgenticChat(user, actor, impersonating, req, resp)
	emit("done", resp)
}
