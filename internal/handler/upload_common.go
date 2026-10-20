package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"panda-backend/internal/models"
	"panda-backend/internal/service"
	"panda-backend/internal/storage"
)

// Dipakai bersama oleh upload manual (menu Evidence/Aplikasi) dan upload via Agentic AI,
// sehingga aturan hak akses & validasi file selalu sama di kedua jalur.

const (
	UploadChannelManual    = "MANUAL"
	UploadChannelAgenticAI = "AGENTIC_AI"
)

// DocCategories adalah kategori dokumen yang valid (urutan = urutan tampilan); sumbernya di models
// agar service (pencarian Agentic AI) memakai daftar yang sama.
var DocCategories = models.DocCategories

func isValidCategory(cat string) bool {
	for _, c := range DocCategories {
		if c == cat {
			return true
		}
	}
	return false
}

// truncateUTF8 memotong s menjadi maksimal max byte tanpa memecah karakter multi-byte.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// ─── Validasi File ───────────────────────────────────────────────────────────

var oleMagic = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1} // .doc/.xls/.ppt lama

func isZipContainer(_ []byte, ct string) bool { return ct == "application/zip" }
func isOLE(data []byte, _ string) bool        { return bytes.HasPrefix(data, oleMagic) }
func isJPEG(_ []byte, ct string) bool         { return ct == "image/jpeg" }
func isPlainText(_ []byte, ct string) bool    { return strings.HasPrefix(ct, "text/plain") }

// allowedUploadTypes: ekstensi yang diterima beserta pengecekan isi file (magic bytes),
// agar file lain yang sekadar di-rename (mis. .exe -> .pdf) ditolak.
var allowedUploadTypes = map[string]func(data []byte, sniffed string) bool{
	".pdf":  func(_ []byte, ct string) bool { return ct == "application/pdf" },
	".docx": isZipContainer,
	".xlsx": isZipContainer,
	".pptx": isZipContainer,
	".doc":  isOLE,
	".xls":  isOLE,
	".ppt":  isOLE,
	".png":  func(_ []byte, ct string) bool { return ct == "image/png" },
	".jpg":  isJPEG,
	".jpeg": isJPEG,
	".txt":  isPlainText,
	".csv":  isPlainText,
	".md":   isPlainText,
}

// AllowedUploadExtensions mengembalikan daftar ekstensi, mis. ".csv, .doc, ..." (untuk pesan error & atribut accept).
func AllowedUploadExtensions() []string {
	exts := make([]string, 0, len(allowedUploadTypes))
	for e := range allowedUploadTypes {
		exts = append(exts, e)
	}
	sort.Strings(exts)
	return exts
}

func maxUploadBytes() int64 {
	mb, err := strconv.Atoi(strings.TrimSpace(os.Getenv("UPLOAD_MAX_MB")))
	if err != nil || mb <= 0 {
		mb = 25
	}
	return int64(mb) << 20
}

type uploadedFile struct {
	Data         []byte
	OriginalName string
	Ext          string
	SHA256       string
}

// parseUploadForm membatasi ukuran request lalu mem-parse multipart form. Harus dipanggil
// sebelum c.PostForm/c.FormFile agar batas ukuran berlaku.
func parseUploadForm(c *gin.Context) (int, string) {
	limit := maxUploadBytes()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit+(1<<20)) // +1 MB untuk field form
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return http.StatusRequestEntityTooLarge, fmt.Sprintf("Ukuran file melebihi batas %d MB", limit>>20)
		}
		return http.StatusBadRequest, "Form upload tidak valid (gunakan multipart/form-data)"
	}
	return 0, ""
}

// readUploadedFile membaca field "file" (wajib) lalu memvalidasinya.
func readUploadedFile(c *gin.Context) (uploadedFile, int, string) {
	fh, err := c.FormFile("file")
	if err != nil || fh == nil {
		return uploadedFile{}, http.StatusBadRequest, "File dokumen wajib dilampirkan"
	}
	if fh.Size > maxUploadBytes() {
		return uploadedFile{}, http.StatusRequestEntityTooLarge, fmt.Sprintf("Ukuran file melebihi batas %d MB", maxUploadBytes()>>20)
	}
	f, err := fh.Open()
	if err != nil {
		return uploadedFile{}, http.StatusBadRequest, "File tidak dapat dibaca"
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return uploadedFile{}, http.StatusBadRequest, "File tidak dapat dibaca"
	}
	return validateUploadedFile(fh.Filename, data, maxUploadBytes())
}

// validateUploadedFile memeriksa ekstensi, ukuran, dan kecocokan isi file dengan ekstensinya.
func validateUploadedFile(name string, data []byte, limit int64) (uploadedFile, int, string) {
	name = filepath.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	if len(name) > 200 {
		// pertahankan bagian akhir (berisi ekstensi), mulai dari awal rune yang utuh
		start := len(name) - 200
		for start < len(name) && !utf8.RuneStart(name[start]) {
			start++
		}
		name = name[start:]
	}
	ext := strings.ToLower(filepath.Ext(name))
	check, ok := allowedUploadTypes[ext]
	if !ok {
		return uploadedFile{}, http.StatusUnsupportedMediaType,
			fmt.Sprintf("Tipe file %q tidak didukung. Gunakan: %s", ext, strings.Join(AllowedUploadExtensions(), ", "))
	}
	if len(data) == 0 {
		return uploadedFile{}, http.StatusBadRequest, "File kosong (0 byte)"
	}
	if int64(len(data)) > limit {
		return uploadedFile{}, http.StatusRequestEntityTooLarge, fmt.Sprintf("Ukuran file melebihi batas %d MB", limit>>20)
	}
	if !check(data, http.DetectContentType(data)) {
		return uploadedFile{}, http.StatusUnsupportedMediaType,
			fmt.Sprintf("Isi file tidak sesuai dengan ekstensi %s. Pastikan file tidak rusak dan tidak sekadar di-rename.", ext)
	}
	sum := sha256.Sum256(data)
	return uploadedFile{Data: data, OriginalName: name, Ext: ext, SHA256: hex.EncodeToString(sum[:])}, 0, ""
}

// ─── Cakupan Folder ──────────────────────────────────────────────────────────

// applyFolderScope membatasi query app_folders sesuai cakupan Tim/Branch user (Admin: semua folder).
// Aturannya ada di service agar tool Agentic AI memakai aturan yang sama.
func applyFolderScope(q *gorm.DB, user models.User) *gorm.DB {
	return service.ApplyFolderScope(q, user)
}

// canAccessFolder adalah versi in-memory dari applyFolderScope untuk satu folder.
func canAccessFolder(user models.User, f models.AppFolder) bool {
	return service.CanAccessFolder(user, f)
}

// ─── Tujuan Upload ───────────────────────────────────────────────────────────

type uploadTarget struct {
	ModuleSource    string // APPLICATION | EVIDENCE
	ApplicationID   string
	SectionID       string
	VisibilityScope string // khusus Admin pada EVIDENCE: ALL_TIM | TEAM_ONLY
	TargetTeamID    string
	TargetBranchID  string
}

type resolvedTarget struct {
	ModuleSource     string
	FolderID         *string
	AppID            *string
	SectionID        *string
	OwnerTeamID      *string
	OwnerBranchID    *string
	VisibilityScope  string
	FolderName       string
	AppName          string
	SectionLabel     string
	SectionVersion   string
	PrefixCode       string
	TeamDisplayLabel string
}

// resolveUploadTarget memvalidasi tujuan upload terhadap hak akses user. Pegawai TI hanya dapat
// mengunggah ke folder aplikasi dalam cakupan timnya, dan evidence-nya selalu milik timnya sendiri.
func (h *Handler) resolveUploadTarget(user models.User, t uploadTarget) (resolvedTarget, int, string) {
	r := resolvedTarget{ModuleSource: t.ModuleSource, VisibilityScope: "TEAM_ONLY", FolderName: "-", AppName: "-", PrefixCode: "EVID"}

	switch t.ModuleSource {
	case "APPLICATION":
		var app models.Application
		if err := h.DB.Preload("Folder").First(&app, "id = ?", strings.TrimSpace(t.ApplicationID)).Error; err != nil || app.Folder == nil {
			return r, http.StatusNotFound, "Aplikasi tujuan tidak ditemukan"
		}
		if !canAccessFolder(user, *app.Folder) {
			return r, http.StatusForbidden, "Aplikasi ini berada di luar cakupan Tim/Branch Anda"
		}
		if strings.TrimSpace(t.SectionID) == "" {
			return r, http.StatusBadRequest, "Section versi aplikasi wajib dipilih"
		}
		var sec models.AppChangeSection
		if err := h.DB.First(&sec, "id = ? AND application_id = ?", strings.TrimSpace(t.SectionID), app.ID).Error; err != nil {
			return r, http.StatusBadRequest, "Section tidak ditemukan pada aplikasi ini"
		}
		r.AppID, r.FolderID, r.SectionID = &app.ID, &app.Folder.ID, &sec.ID
		r.OwnerTeamID, r.OwnerBranchID = &app.Folder.AssignedTeamID, app.Folder.AssignedBranchID
		r.FolderName, r.AppName, r.PrefixCode = app.Folder.FolderName, app.AppName, app.AppCode
		r.SectionVersion = sec.VersionLabel
		r.SectionLabel = fmt.Sprintf("%s - %s", sec.VersionLabel, sec.SectionTitle)

	case "EVIDENCE":
		if user.IsAdmin() {
			vis := strings.ToUpper(strings.TrimSpace(t.VisibilityScope))
			team := strings.TrimSpace(t.TargetTeamID)
			if vis == "ALL_TIM" || team == "" || team == "ALL_TIM" {
				r.VisibilityScope = "ALL_TIM"
			} else {
				var tm models.MasterTeam
				if err := h.DB.First(&tm, "id = ?", team).Error; err != nil {
					return r, http.StatusBadRequest, "Tim tujuan evidence tidak ditemukan"
				}
				r.OwnerTeamID = &tm.ID
				if branch := strings.TrimSpace(t.TargetBranchID); branch != "" {
					var br models.MasterBranch
					if err := h.DB.First(&br, "id = ? AND team_id = ?", branch, tm.ID).Error; err != nil {
						return r, http.StatusBadRequest, "Branch tujuan tidak ditemukan pada tim tersebut"
					}
					r.OwnerBranchID = &br.ID
				}
			}
		} else {
			// Pegawai TI: field target diabaikan, evidence selalu milik tim/branch-nya sendiri
			r.OwnerTeamID, r.OwnerBranchID = user.TeamID, user.BranchID
		}

	default:
		return r, http.StatusBadRequest, "Modul tujuan harus APPLICATION atau EVIDENCE"
	}

	r.TeamDisplayLabel = h.ownerDisplayLabel(user, r)
	return r, 0, ""
}

func (h *Handler) ownerDisplayLabel(user models.User, r resolvedTarget) string {
	if r.VisibilityScope == "ALL_TIM" {
		return "Semua Tim (All Tim) • Diunggah oleh " + user.FullName
	}
	if r.OwnerTeamID == nil {
		return formatUserTeamLabel(user)
	}
	var t models.MasterTeam
	if err := h.DB.First(&t, "id = ?", *r.OwnerTeamID).Error; err != nil {
		return formatUserTeamLabel(user)
	}
	if r.OwnerBranchID != nil {
		var b models.MasterBranch
		if err := h.DB.First(&b, "id = ?", *r.OwnerBranchID).Error; err == nil {
			return fmt.Sprintf("%s • %s", t.TeamName, b.BranchName)
		}
	}
	return t.TeamName
}

// ─── Metadata & Penyimpanan Dokumen ──────────────────────────────────────────

type documentMeta struct {
	Category      string
	Title         string
	ITDomain      string
	VersionLabel  string
	AuditTags     string
	ValidUntil    string
	Summary       string
	UploadChannel string // MANUAL | AGENTIC_AI
	ActionType    string // tipe log audit
	LogNote       string // tambahan detail log (mis. saran AI vs pilihan user)
}

// normalizeDocumentMeta mengisi nilai default & memvalidasi kategori.
func normalizeDocumentMeta(m *documentMeta, r resolvedTarget, user models.User) (int, string) {
	m.Category = strings.ToUpper(strings.TrimSpace(m.Category))
	if m.Category == "" {
		m.Category = "LAINNYA"
	}
	if !isValidCategory(m.Category) {
		return http.StatusBadRequest, "Kategori dokumen tidak dikenal"
	}
	m.Title = strings.TrimSpace(m.Title)
	if m.Title == "" {
		m.Title = "Dokumen Audit " + m.Category
	}
	m.Title = truncateUTF8(m.Title, 250)
	if m.ITDomain = strings.TrimSpace(m.ITDomain); m.ITDomain == "" {
		m.ITDomain = "IT Governance & Audit"
	}
	m.VersionLabel = strings.TrimSpace(m.VersionLabel)
	if r.SectionVersion != "" {
		m.VersionLabel = r.SectionVersion // dokumen aplikasi mengikuti versi section tujuan
	}
	if m.VersionLabel == "" {
		m.VersionLabel = "v1.0"
	}
	if m.AuditTags = strings.TrimSpace(m.AuditTags); m.AuditTags == "" {
		m.AuditTags = fmt.Sprintf("AUDIT-INT-%d", time.Now().Year())
	}
	if m.ValidUntil = strings.TrimSpace(m.ValidUntil); m.ValidUntil == "" {
		m.ValidUntil = "Permanen (" + m.VersionLabel + ")"
	}
	if m.Summary = strings.TrimSpace(m.Summary); m.Summary == "" {
		m.Summary = fmt.Sprintf("Dokumen [%s] %s (%s) untuk %s. Diunggah oleh %s (Badge: %s) pada %s WIB. Berisi bukti kelengkapan administrasi dan teknis yang siap diverifikasi untuk audit TI.",
			m.Category, m.Title, m.VersionLabel, r.TeamDisplayLabel, user.FullName, user.BadgeNumber, time.Now().Format("02 Jan 2006 15:04:05"))
	}
	if m.UploadChannel == "" {
		m.UploadChannel = UploadChannelManual
	}
	if m.ActionType == "" {
		m.ActionType = "UPLOAD_EVIDENCE"
		if r.ModuleSource == "APPLICATION" {
			m.ActionType = "UPLOAD_APP_DOC"
		}
	}
	return 0, ""
}

// persistDocument menyimpan file fisik (MinIO bila kredensialnya terisi, selain itu folder lokal), metadata,
// indeks isi (RAG), dan log audit. Bila file gagal disimpan, upload gagal; bila metadata gagal disimpan, file dihapus lagi.
func (h *Handler) persistDocument(user models.User, r resolvedTarget, m documentMeta, f uploadedFile) (models.Document, models.UploadAuditLog, service.ContentIndexResult, error) {
	var empty service.ContentIndexResult
	ctx := context.Background()

	now := time.Now()
	stdName := fmt.Sprintf("%s_%s_%s_%s%s", r.PrefixCode, m.Category, strings.ReplaceAll(m.VersionLabel, " ", ""), now.Format("20060102_150405"), f.Ext)
	exists, err := storage.Exists(ctx, stdName)
	if err != nil {
		return models.Document{}, models.UploadAuditLog{}, empty, fmt.Errorf("storage dokumen tidak dapat diakses: %w", err)
	}
	if exists {
		// Dua upload pada detik yang sama: beri akhiran unik agar file lama tidak tertimpa
		stdName = strings.TrimSuffix(stdName, f.Ext) + "_" + uuid.NewString()[:8] + f.Ext
	}
	savePath, err := storage.Save(ctx, stdName, f.Data)
	if err != nil {
		return models.Document{}, models.UploadAuditLog{}, empty, fmt.Errorf("file gagal disimpan ke storage: %w", err)
	}

	sectionLabel := r.SectionLabel
	if sectionLabel == "" {
		sectionLabel = m.VersionLabel
	}

	doc := models.Document{
		ID:                  uuid.NewString(),
		ModuleSource:        r.ModuleSource,
		FolderID:            r.FolderID,
		ApplicationID:       r.AppID,
		SectionID:           r.SectionID,
		DocCategory:         m.Category,
		ITDomain:            m.ITDomain,
		Title:               m.Title,
		StandardizedName:    stdName,
		OriginalFilename:    f.OriginalName,
		FileExtension:       f.Ext,
		FilePath:            savePath,
		FileSizeBytes:       int64(len(f.Data)),
		FileHashSHA256:      f.SHA256,
		VersionLabel:        m.VersionLabel,
		AuditTags:           m.AuditTags,
		ValidUntil:          m.ValidUntil,
		ValidityStatus:      "VALID",
		VisibilityScope:     r.VisibilityScope,
		OwnerTeamID:         r.OwnerTeamID,
		OwnerBranchID:       r.OwnerBranchID,
		AISummary:           m.Summary,
		UploadChannel:       m.UploadChannel,
		UploadedByBadge:     user.BadgeNumber,
		UploadedByName:      user.FullName,
		UploadedByRole:      user.Role,
		UploadedByTeamLabel: r.TeamDisplayLabel,
		UploadedAt:          now,
	}
	if err := h.DB.Create(&doc).Error; err != nil {
		_ = storage.Remove(ctx, savePath)
		return models.Document{}, models.UploadAuditLog{}, empty, fmt.Errorf("metadata dokumen gagal disimpan: %w", err)
	}

	// Baca isi file asli & pecah menjadi potongan untuk pencarian Agentic AI (RAG)
	contentIndex := service.IndexDocumentContent(h.DB, &doc, f.Data)

	detail := fmt.Sprintf("%s (Badge %s) mengunggah dokumen [%s] '%s' (%s) pada %s", user.FullName, user.BadgeNumber, doc.DocCategory, doc.Title, doc.StandardizedName, now.Format("02 Jan 2006 15:04:05 WIB"))
	if m.LogNote != "" {
		detail += " — " + m.LogNote
	}
	detail = truncateUTF8(detail, 1000)
	logEntry := models.UploadAuditLog{
		BadgeNumber:        user.BadgeNumber,
		UserName:           user.FullName,
		UserRole:           user.Role,
		TeamAndBranchLabel: formatUserTeamLabel(user),
		ActionType:         m.ActionType,
		ModuleSource:       r.ModuleSource,
		DocumentID:         &doc.ID,
		FolderName:         r.FolderName,
		AppName:            r.AppName,
		SectionVersion:     sectionLabel,
		DocumentTitle:      doc.Title,
		VisibilityLabel:    r.TeamDisplayLabel,
		DetailDescription:  detail,
		ActionTimestamp:    now,
	}
	h.DB.Create(&logEntry)

	return doc, logEntry, contentIndex, nil
}
