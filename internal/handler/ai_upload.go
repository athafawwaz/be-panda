package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"panda-backend/internal/models"
	"panda-backend/internal/service"
	"panda-backend/internal/storage"
)

// ─── Upload Dokumen via PANDA Agentic AI (Analisis → Konfirmasi) ─────────────
// 1. analyze: file disimpan di staging privat, AI menyusun PILIHAN tujuan & kategori berperingkat.
// 2. confirm: user memilih sendiri; server memvalidasi ulang hak akses lalu menyimpan dokumen.
// Saat impersonate, kedua endpoint otomatis ditolak middleware (mode read-only).

const stagingTTL = time.Hour

// stagingKeyPrefix: file staging upload via AI disimpan di storage yang sama dengan dokumen (folder lokal
// uploads/_staging atau prefix _staging/ di bucket MinIO).
const stagingKeyPrefix = "_staging/"

// rateLimiter sederhana per user agar analisis (yang dapat memanggil LLM) tidak disalahgunakan.
type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-l.window)
	recent := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= l.limit {
		l.hits[key] = recent
		return false
	}
	l.hits[key] = append(recent, time.Now())
	return true
}

var analyzeLimiter = &rateLimiter{hits: map[string][]time.Time{}, limit: 10, window: time.Minute}

// uploadCandidates membangun daftar tujuan yang BOLEH dipilih user: menu Evidence + setiap section
// aplikasi pada folder dalam cakupan Tim/Branch-nya. AI hanya boleh mengurutkan daftar ini.
func (h *Handler) uploadCandidates(user models.User) []service.UploadCandidate {
	evidenceLabel := "Menu Evidence — " + formatUserTeamLabel(user)
	if user.IsAdmin() {
		evidenceLabel = "Menu Evidence (visibilitas dipilih saat konfirmasi)"
	}
	cands := []service.UploadCandidate{{
		Key:        service.EvidenceDestinationKey,
		Kind:       "EVIDENCE",
		Label:      evidenceLabel,
		OwnerLabel: formatUserTeamLabel(user),
	}}

	var folders []models.AppFolder
	applyFolderScope(h.DB.Model(&models.AppFolder{}), user).
		Preload("AssignedTeam").
		Preload("AssignedBranch").
		Preload("Applications").
		Preload("Applications.Sections", func(db *gorm.DB) *gorm.DB {
			return db.Order("sort_order DESC, created_at DESC")
		}).
		Order("folder_name ASC").
		Find(&folders)

	for _, f := range folders {
		owner := "-"
		if f.AssignedTeam != nil {
			owner = f.AssignedTeam.TeamName
			if f.AssignedBranch != nil {
				owner += " • " + f.AssignedBranch.BranchName
			}
		}
		for _, app := range f.Applications {
			for i, sec := range app.Sections {
				secLabel := fmt.Sprintf("%s - %s", sec.VersionLabel, sec.SectionTitle)
				cands = append(cands, service.UploadCandidate{
					Key:           fmt.Sprintf("app:%s:%s", app.ID, sec.ID),
					Kind:          "APPLICATION",
					Label:         fmt.Sprintf("%s › %s (%s) › %s", f.FolderName, app.AppName, app.AppCode, secLabel),
					FolderID:      f.ID,
					FolderName:    f.FolderName,
					ApplicationID: app.ID,
					AppCode:       app.AppCode,
					AppName:       app.AppName,
					SectionID:     sec.ID,
					SectionLabel:  secLabel,
					VersionLabel:  sec.VersionLabel,
					OwnerLabel:    owner,
					LatestSection: i == 0,
				})
			}
		}
	}
	return cands
}

func (h *Handler) discardStaging(st models.UploadStaging) {
	if err := storage.Remove(context.Background(), st.FilePath); err != nil {
		log.Printf("[AI Upload] gagal menghapus file staging %s: %v", st.FilePath, err)
	}
	h.DB.Delete(&st)
}

func (h *Handler) AnalyzeAIUpload(c *gin.Context) {
	user := getCurrentUser(c)
	if !analyzeLimiter.allow(user.ID) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Terlalu banyak analisis upload dalam 1 menit. Coba lagi sebentar."})
		return
	}
	if status, msg := parseUploadForm(c); status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}
	file, status, msg := readUploadedFile(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}
	message := truncateUTF8(strings.TrimSpace(c.PostForm("message")), 1000)

	stagingID := uuid.NewString()
	stagingPath, err := storage.Save(c.Request.Context(), stagingKeyPrefix+stagingID+file.Ext, file.Data)
	if err != nil {
		log.Printf("[AI Upload] gagal menulis staging (%s): %v", storage.Describe(), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "File gagal disimpan sementara"})
		return
	}

	text, extractErr := service.ExtractDocumentText(file.Data, file.Ext)
	cands := h.uploadCandidates(user)
	sugg := service.SuggestUpload(service.LoadLLMConfig(h.DB), service.SuggestUploadInput{
		FileName:      file.OriginalName,
		Message:       message,
		ExtractedText: text,
		Categories:    DocCategories,
		Candidates:    cands,
	})
	if strings.TrimSpace(text) == "" {
		if errors.Is(extractErr, service.ErrUnsupportedFormat) {
			sugg.ContentNote = "Isi file tipe ini belum bisa dibaca, saran hanya dari nama file & pesan Anda."
		} else {
			sugg.ContentNote = "File tidak berisi teks yang terbaca (mungkin hasil scan), saran hanya dari nama file & pesan Anda."
		}
	}

	// Simpan saran AI untuk dicatat di log audit sebagai pembanding pilihan akhir user
	suggestionJSON, _ := json.Marshal(gin.H{"destinations": sugg.Destinations, "categories": sugg.Categories, "engine": sugg.Engine})
	staging := models.UploadStaging{
		ID:             stagingID,
		UserID:         user.ID,
		BadgeNumber:    user.BadgeNumber,
		FilePath:       stagingPath,
		OriginalName:   file.OriginalName,
		FileExtension:  file.Ext,
		FileSizeBytes:  int64(len(file.Data)),
		FileHashSHA256: file.SHA256,
		UserMessage:    message,
		SuggestionJSON: string(suggestionJSON),
		ExpiresAt:      time.Now().Add(stagingTTL),
	}
	if err := h.DB.Create(&staging).Error; err != nil {
		_ = storage.Remove(context.Background(), stagingPath)
		log.Printf("[AI Upload] gagal menyimpan sesi staging: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Sesi upload gagal dibuat"})
		return
	}
	// Kedaluwarsa ditulis & dibandingkan memakai jam SQL Server: driver membaca DATETIME2 sebagai UTC,
	// sehingga perbandingan dengan time.Now() di Go bisa meleset sebesar selisih zona waktu.
	h.DB.Model(&models.UploadStaging{}).Where("id = ?", staging.ID).
		Update("expires_at", gorm.Expr("DATEADD(second, ?, SYSDATETIME())", int(stagingTTL.Seconds())))

	var duplicate gin.H
	if dup := service.FindDuplicateInScope(h.DB, user, file.SHA256); dup != nil {
		duplicate = gin.H{
			"id":                dup.ID,
			"title":             dup.Title,
			"standardized_name": dup.StandardizedName,
			"doc_category":      dup.DocCategory,
			"version_label":     dup.VersionLabel,
			"module_source":     dup.ModuleSource,
			"uploaded_by_name":  dup.UploadedByName,
			"uploaded_by_badge": dup.UploadedByBadge,
			"uploaded_at":       dup.UploadedAt,
		}
	}

	c.JSON(http.StatusCreated, gin.H{
		"staging_id": staging.ID,
		"expires_at": staging.ExpiresAt,
		"file": gin.H{
			"name":       file.OriginalName,
			"size_bytes": len(file.Data),
			"extension":  file.Ext,
			"sha256":     file.SHA256,
		},
		"message":    message,
		"suggestion": sugg,
		"candidates": cands,
		"categories": DocCategories,
		"duplicate":  duplicate,
		"is_admin":   user.IsAdmin(),
	})
}

type confirmAIUploadRequest struct {
	StagingID       string `json:"staging_id" binding:"required"`
	DestinationKey  string `json:"destination_key" binding:"required"`
	Category        string `json:"category" binding:"required"`
	Title           string `json:"title"`
	VersionLabel    string `json:"version_label"`
	ITDomain        string `json:"it_domain"`
	AuditTags       string `json:"audit_tags"`
	ValidUntil      string `json:"valid_until"`
	Summary         string `json:"summary"`
	VisibilityScope string `json:"visibility_scope"`
	TargetTeamID    string `json:"target_team_id"`
	TargetBranchID  string `json:"target_branch_id"`
}

// parseDestinationKey mengubah kunci tujuan ("evidence" | "app:<appID>:<sectionID>") menjadi uploadTarget.
func parseDestinationKey(key string) (uploadTarget, bool) {
	key = strings.TrimSpace(key)
	if key == service.EvidenceDestinationKey {
		return uploadTarget{ModuleSource: "EVIDENCE"}, true
	}
	parts := strings.Split(key, ":")
	if len(parts) == 3 && parts[0] == "app" && parts[1] != "" && parts[2] != "" {
		return uploadTarget{ModuleSource: "APPLICATION", ApplicationID: parts[1], SectionID: parts[2]}, true
	}
	return uploadTarget{}, false
}

// aiChoiceNote merangkum saran AI vs pilihan akhir user untuk log audit.
func aiChoiceNote(suggestionJSON, destKey, category string) string {
	var s struct {
		Destinations []service.RankedOption `json:"destinations"`
		Categories   []service.RankedOption `json:"categories"`
		Engine       string                 `json:"engine"`
	}
	_ = json.Unmarshal([]byte(suggestionJSON), &s)
	rank := func(opts []service.RankedOption, value string) string {
		for i, o := range opts {
			if o.Value == value {
				return fmt.Sprintf("saran AI #%d", i+1)
			}
		}
		return "dipilih manual"
	}
	return fmt.Sprintf("via Agentic AI (%s): tujuan %s, kategori %s %s",
		s.Engine, rank(s.Destinations, destKey), category, rank(s.Categories, category))
}

func (h *Handler) ConfirmAIUpload(c *gin.Context) {
	user := getCurrentUser(c)
	var req confirmAIUploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Tujuan dan kategori dokumen wajib dipilih"})
		return
	}

	var st models.UploadStaging
	if err := h.DB.First(&st, "id = ? AND user_id = ?", req.StagingID, user.ID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Sesi upload tidak ditemukan atau sudah dibatalkan"})
		return
	}
	var alive int64
	h.DB.Model(&models.UploadStaging{}).Where("id = ? AND expires_at > SYSDATETIME()", st.ID).Count(&alive)
	if alive == 0 {
		h.discardStaging(st)
		c.JSON(http.StatusGone, gin.H{"error": "Sesi upload sudah kedaluwarsa, silakan lampirkan ulang file"})
		return
	}

	target, ok := parseDestinationKey(req.DestinationKey)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Tujuan upload tidak valid"})
		return
	}
	target.VisibilityScope, target.TargetTeamID, target.TargetBranchID = req.VisibilityScope, req.TargetTeamID, req.TargetBranchID

	// Hak akses divalidasi ulang dengan aturan yang sama seperti upload manual
	resolved, status, msg := h.resolveUploadTarget(user, target)
	if status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	data, err := storage.Read(c.Request.Context(), st.FilePath)
	if err != nil {
		log.Printf("[AI Upload] file staging %s tidak terbaca: %v", st.FilePath, err)
		h.discardStaging(st)
		c.JSON(http.StatusGone, gin.H{"error": "File sementara sudah tidak tersedia, silakan lampirkan ulang"})
		return
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != st.FileHashSHA256 {
		h.discardStaging(st)
		c.JSON(http.StatusConflict, gin.H{"error": "File sementara berubah sejak dianalisis, silakan lampirkan ulang"})
		return
	}

	meta := documentMeta{
		Category:      req.Category,
		Title:         req.Title,
		ITDomain:      req.ITDomain,
		VersionLabel:  req.VersionLabel,
		AuditTags:     req.AuditTags,
		ValidUntil:    req.ValidUntil,
		Summary:       req.Summary,
		UploadChannel: UploadChannelAgenticAI,
		ActionType:    "UPLOAD_VIA_AI",
		LogNote:       aiChoiceNote(st.SuggestionJSON, req.DestinationKey, strings.ToUpper(strings.TrimSpace(req.Category))),
	}
	if status, msg := normalizeDocumentMeta(&meta, resolved, user); status != 0 {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	file := uploadedFile{Data: data, OriginalName: st.OriginalName, Ext: st.FileExtension, SHA256: st.FileHashSHA256}
	doc, logEntry, contentIndex, err := h.persistDocument(user, resolved, meta, file)
	if err != nil {
		log.Printf("[AI Upload] %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menyimpan dokumen. Silakan coba lagi."})
		return
	}
	h.discardStaging(st)

	c.JSON(http.StatusCreated, gin.H{
		"document":      doc,
		"log":           logEntry,
		"content_index": contentIndex,
		"location":      resolved.TeamDisplayLabel,
	})
}

func (h *Handler) CancelAIUpload(c *gin.Context) {
	user := getCurrentUser(c)
	var st models.UploadStaging
	if err := h.DB.First(&st, "id = ? AND user_id = ?", c.Param("id"), user.ID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Sesi upload tidak ditemukan"})
		return
	}
	h.discardStaging(st)
	c.JSON(http.StatusOK, gin.H{"message": "Upload dibatalkan, file sementara dihapus"})
}
