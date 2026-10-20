package handler

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"panda-backend/internal/models"
	"panda-backend/internal/service"
)

const (
	chatTitleMaxRunes    = 80
	chatSessionListLimit = 100
)

// chatTitle membuat judul percakapan dari pertanyaan pertama.
func chatTitle(prompt string) string {
	title := []rune(strings.Join(strings.Fields(prompt), " "))
	if len(title) > chatTitleMaxRunes {
		return string(title[:chatTitleMaxRunes]) + "…"
	}
	return string(title)
}

// replyExcerptRunes: panjang cuplikan pesan yang dibalas/diteruskan yang disimpan untuk ditampilkan di riwayat.
const replyExcerptRunes = 300

// saveChatTurn menyimpan satu tanya-jawab Agentic AI (beserta langkah AI & pesan yang dibalas) ke riwayat chat
// milik owner. Sesi baru dibuat bila sessionID kosong atau bukan milik owner; id sesi & id pesan dikembalikan.
func (h *Handler) saveChatTurn(owner models.User, sessionID string, req service.AIChatRequest, resp *service.AIChatResponse) (string, int64, error) {
	now := time.Now().UTC()
	docsJSON, err := json.Marshal(resp.MatchedDocuments)
	if err != nil {
		return "", 0, err
	}
	traceJSON := ""
	if resp.Trace != nil {
		if b, err := json.Marshal(resp.Trace); err == nil {
			traceJSON = string(b)
		}
	}
	replyJSON := ""
	if req.ReplyTo != nil {
		ref := *req.ReplyTo
		if r := []rune(strings.TrimSpace(ref.Text)); len(r) > replyExcerptRunes {
			ref.Text = string(r[:replyExcerptRunes]) + "…"
		}
		if b, err := json.Marshal(ref); err == nil {
			replyJSON = string(b)
		}
	}
	var messageID int64

	err = h.DB.Transaction(func(tx *gorm.DB) error {
		var session models.AIChatSession
		if sessionID == "" || tx.First(&session, "id = ? AND user_id = ?", sessionID, owner.ID).Error != nil {
			session = models.AIChatSession{
				ID:        uuid.NewString(),
				UserID:    owner.ID,
				Title:     chatTitle(req.Prompt),
				CreatedAt: now,
				UpdatedAt: now,
			}
			if err := tx.Create(&session).Error; err != nil {
				return err
			}
		}
		sessionID = session.ID

		msg := models.AIChatMessage{
			SessionID:            session.ID,
			Prompt:               req.Prompt,
			FilterModule:         req.FilterModule,
			ReplyMarkdown:        resp.ReplyMarkdown,
			EngineMode:           resp.EngineMode,
			ScopeLabel:           resp.ScopeLabel,
			MatchedDocumentsJSON: string(docsJSON),
			TraceJSON:            traceJSON,
			ReplyToJSON:          replyJSON,
			AnswerBasis:          resp.AnswerBasis,
			CreatedAt:            now,
		}
		if err := tx.Create(&msg).Error; err != nil {
			return err
		}
		messageID = msg.ID
		return tx.Model(&models.AIChatSession{}).Where("id = ?", session.ID).Updates(map[string]interface{}{
			"updated_at":    now,
			"message_count": gorm.Expr("message_count + 1"),
		}).Error
	})
	return sessionID, messageID, err
}

// GetChatSessions menampilkan daftar percakapan Agentic AI milik user yang login, terbaru di atas.
func (h *Handler) GetChatSessions(c *gin.Context) {
	owner, _ := getRealUser(c)
	var sessions []models.AIChatSession
	if err := h.DB.Where("user_id = ?", owner.ID).
		Order("updated_at DESC").
		Limit(chatSessionListLimit).
		Find(&sessions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal memuat riwayat chat"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"sessions": sessions})
}

type chatTurn struct {
	ID        int64                  `json:"id"`
	Question  string                 `json:"question"`
	Response  service.AIChatResponse `json:"response"`
	ReplyTo   *service.ChatReplyRef  `json:"reply_to,omitempty"` // pesan yang dibalas / diteruskan
	CreatedAt time.Time              `json:"created_at"`
}

// GetChatSession membuka kembali satu percakapan beserta seluruh tanya-jawab dan dokumen yang dikutip.
func (h *Handler) GetChatSession(c *gin.Context) {
	owner, _ := getRealUser(c)
	var session models.AIChatSession
	if err := h.DB.First(&session, "id = ? AND user_id = ?", c.Param("id"), owner.ID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Riwayat chat tidak ditemukan"})
		return
	}

	var messages []models.AIChatMessage
	h.DB.Where("session_id = ?", session.ID).Order("id ASC").Find(&messages)

	turns := make([]chatTurn, 0, len(messages))
	for _, m := range messages {
		docs := []service.AIDocumentCitation{}
		_ = json.Unmarshal([]byte(m.MatchedDocumentsJSON), &docs)
		var trace *service.AgentTrace
		if m.TraceJSON != "" {
			var t service.AgentTrace
			if json.Unmarshal([]byte(m.TraceJSON), &t) == nil {
				trace = &t
			}
		}
		var replyTo *service.ChatReplyRef
		if m.ReplyToJSON != "" {
			var r service.ChatReplyRef
			if json.Unmarshal([]byte(m.ReplyToJSON), &r) == nil {
				replyTo = &r
			}
		}
		turns = append(turns, chatTurn{
			ID:       m.ID,
			Question: m.Prompt,
			Response: service.AIChatResponse{
				ReplyMarkdown:    m.ReplyMarkdown,
				EngineMode:       m.EngineMode,
				ScopeLabel:       m.ScopeLabel,
				MatchedDocuments: docs,
				QueriedAt:        m.CreatedAt,
				SessionID:        session.ID,
				MessageID:        m.ID,
				Trace:            trace,
				AnswerBasis:      m.AnswerBasis,
			},
			ReplyTo:   replyTo,
			CreatedAt: m.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"session": session, "messages": turns})
}

// DeleteChatSession menghapus satu percakapan milik user beserta seluruh isinya.
func (h *Handler) DeleteChatSession(c *gin.Context) {
	owner, _ := getRealUser(c)
	sessionID := c.Param("id")

	found := false
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Where("id = ? AND user_id = ?", sessionID, owner.ID).Delete(&models.AIChatSession{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		found = true
		return tx.Where("session_id = ?", sessionID).Delete(&models.AIChatMessage{}).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menghapus riwayat chat"})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "Riwayat chat tidak ditemukan"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

func cleanDocShortTitle(doc models.Document) string {
	if doc.Application != nil && strings.TrimSpace(doc.Application.AppName) != "" {
		return strings.TrimSpace(doc.Application.AppName)
	}
	t := strings.TrimSpace(doc.Title)
	runes := []rune(t)
	if len(runes) > 36 {
		trimmed := string(runes[:36])
		if lastSpace := strings.LastIndex(trimmed, " "); lastSpace > 20 {
			trimmed = trimmed[:lastSpace]
		}
		return strings.TrimRight(trimmed, " (.,-–_") + "…"
	}
	return t
}

func generateDynamicShortcuts(docs []models.Document, user models.User) []string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	fallbackShortcuts := []string{
		"Carikan dokumen SOP Tata Kelola All Tim",
		"Tampilkan BAST versi awal v1.0",
		"Cari laporan SLA Layanan TI",
		"Cari laporan uji DRC Infrastruktur",
		"Carikan dokumen UAT aplikasi terbaru",
		"Tampilkan SOP penanganan insiden keamanan TI",
		"Cek dokumen pemenuhan kepatuhan audit TI",
	}
	r.Shuffle(len(fallbackShortcuts), func(i, j int) {
		fallbackShortcuts[i], fallbackShortcuts[j] = fallbackShortcuts[j], fallbackShortcuts[i]
	})

	if len(docs) == 0 {
		return fallbackShortcuts[:5]
	}

	shuffledDocs := make([]models.Document, len(docs))
	copy(shuffledDocs, docs)
	r.Shuffle(len(shuffledDocs), func(i, j int) {
		shuffledDocs[i], shuffledDocs[j] = shuffledDocs[j], shuffledDocs[i]
	})

	seen := make(map[string]bool)
	var results []string

	for _, doc := range shuffledDocs {
		shortName := cleanDocShortTitle(doc)
		cat := strings.ToUpper(strings.TrimSpace(doc.DocCategory))

		var candidates []string
		switch cat {
		case "UAT":
			candidates = []string{
				fmt.Sprintf("Carikan dokumen UAT %s", shortName),
				fmt.Sprintf("Bagaimana hasil pengujian UAT %s?", shortName),
				fmt.Sprintf("Tampilkan ringkasan UAT %s", shortName),
				fmt.Sprintf("Cek skenario uji UAT pada %s", shortName),
			}
		case "BAST":
			ver := doc.VersionLabel
			if ver == "" {
				ver = "terbaru"
			}
			candidates = []string{
				fmt.Sprintf("Tampilkan BAST %s versi %s", shortName, ver),
				fmt.Sprintf("Carikan dokumen BAST serah terima %s", shortName),
				fmt.Sprintf("Cek berita acara serah terima %s", shortName),
			}
		case "SOP":
			candidates = []string{
				fmt.Sprintf("Tampilkan SOP terkait %s", shortName),
				fmt.Sprintf("Bagaimana prosedur standar %s?", shortName),
				fmt.Sprintf("Periksa kepatuhan SOP %s", shortName),
				fmt.Sprintf("Carikan dokumen SOP %s", shortName),
			}
		case "SLA":
			candidates = []string{
				fmt.Sprintf("Cari laporan SLA %s", shortName),
				fmt.Sprintf("Periksa pemenuhan target SLA pada %s", shortName),
				fmt.Sprintf("Bagaimana realisasi SLA %s?", shortName),
			}
		case "DRC":
			candidates = []string{
				fmt.Sprintf("Tampilkan laporan uji DRC %s", shortName),
				fmt.Sprintf("Bagaimana hasil simulasi DRC %s?", shortName),
				fmt.Sprintf("Carikan bukti pengujian DRC %s", shortName),
			}
		case "PRD":
			candidates = []string{
				fmt.Sprintf("Carikan dokumen PRD %s", shortName),
				fmt.Sprintf("Tampilkan spesifikasi kebutuhan PRD %s", shortName),
				fmt.Sprintf("Cek dokumen PRD versi %s", doc.VersionLabel),
			}
		default:
			candidates = []string{
				fmt.Sprintf("Carikan dokumen %s", shortName),
				fmt.Sprintf("Apa ringkasan audit untuk %s?", shortName),
				fmt.Sprintf("Periksa bukti audit dokumen %s", shortName),
				fmt.Sprintf("Tampilkan dokumen %s (%s)", shortName, doc.DocCategory),
			}
		}

		if doc.AuditTags != "" {
			parts := strings.Split(doc.AuditTags, ",")
			if len(parts) > 0 {
				first := strings.TrimSpace(parts[0])
				if len(first) > 0 && len([]rune(first)) <= 25 {
					candidates = append(candidates, fmt.Sprintf("Jelaskan aspek %s pada %s", first, shortName))
				}
			}
		}

		r.Shuffle(len(candidates), func(i, j int) {
			candidates[i], candidates[j] = candidates[j], candidates[i]
		})
		for _, c := range candidates {
			if !seen[c] {
				seen[c] = true
				results = append(results, c)
				break
			}
		}

		if len(results) >= 5 {
			break
		}
	}

	for _, fb := range fallbackShortcuts {
		if len(results) >= 5 {
			break
		}
		if !seen[fb] {
			seen[fb] = true
			results = append(results, fb)
		}
	}

	return results
}

// GetAIChatShortcuts menghasilkan daftar saran shortcut pertanyaan dinamis
// yang disesuaikan secara cerdas berdasarkan dokumen yang boleh diakses pengguna saat ini.
// Setiap pemanggilan / refresh menghasilkan variasi pertanyaan yang berbeda.
func (h *Handler) GetAIChatShortcuts(c *gin.Context) {
	user := getCurrentUser(c)

	// Ambil dokumen yang boleh diakses user sesuai isolasi data Tim/Branch
	query := h.DB.Model(&models.Document{}).
		Preload("Application").
		Preload("OwnerTeam").
		Preload("OwnerBranch")

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
	if err := query.Order("uploaded_at DESC").Limit(50).Find(&docs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal membaca dokumen"})
		return
	}

	shortcuts := generateDynamicShortcuts(docs, user)
	c.JSON(http.StatusOK, gin.H{"shortcuts": shortcuts})
}
