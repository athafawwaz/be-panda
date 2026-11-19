package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"gorm.io/gorm"

	"panda-backend/internal/models"
	"panda-backend/internal/service"
)

const chatSessionListLimit = 100

// GetChatSessions menampilkan daftar percakapan Agentic AI milik user yang login, terbaru di atas.
func (h *Handler) GetChatSessions(c fiber.Ctx) error {
	owner, _ := getRealUser(c)
	var sessions []models.AIChatSession
	if err := h.DB.WithContext(c.Context()).Where("user_id = ?", owner.ID).
		Order("updated_at DESC").
		Limit(chatSessionListLimit).
		Find(&sessions).Error; err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Gagal memuat riwayat chat"})

	}
	return c.Status(http.StatusOK).JSON(fiber.Map{"sessions": sessions})
}

type chatTurn struct {
	ID        int64                  `json:"id"`
	Question  string                 `json:"question"`
	Response  service.AIChatResponse `json:"response"`
	ReplyTo   *service.ChatReplyRef  `json:"reply_to,omitempty"` // pesan yang dibalas / diteruskan
	CreatedAt time.Time              `json:"created_at"`
}

// GetChatSession membuka kembali satu percakapan beserta seluruh tanya-jawab dan dokumen yang dikutip.
func (h *Handler) GetChatSession(c fiber.Ctx) error {
	owner, _ := getRealUser(c)
	var session models.AIChatSession
	if err := h.DB.WithContext(c.Context()).First(&session, "id = ? AND user_id = ?", c.Params("id"), owner.ID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "Riwayat chat tidak ditemukan"})
		} else {
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Gagal memuat riwayat chat"})
		}

	}

	var messages []models.AIChatMessage
	if err := h.DB.WithContext(c.Context()).Where("session_id = ?", session.ID).Order("id ASC").Find(&messages).Error; err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Gagal memuat riwayat chat"})

	}

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
	return c.Status(http.StatusOK).JSON(fiber.Map{"session": session, "messages": turns})
}

// DeleteChatSession menghapus satu percakapan milik user beserta seluruh isinya.
func (h *Handler) DeleteChatSession(c fiber.Ctx) error {
	owner, _ := getRealUser(c)
	sessionID := c.Params("id")

	found := false
	err := h.DB.WithContext(c.Context()).Transaction(func(tx *gorm.DB) error {
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
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Gagal menghapus riwayat chat"})

	}
	if !found {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "Riwayat chat tidak ditemukan"})

	}
	return c.Status(http.StatusOK).JSON(fiber.Map{"deleted": true})
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
func (h *Handler) GetAIChatShortcuts(c fiber.Ctx) error {
	user := getCurrentUser(c)

	// Ambil dokumen yang boleh diakses user sesuai isolasi data Tim/Branch
	query := h.DB.Model(&models.Document{}).
		Preload("Application").
		Preload("OwnerTeam").
		Preload("OwnerBranch")

	query = service.ApplyDocumentScope(query, user)

	var docs []models.Document
	if err := query.Order("uploaded_at DESC").Limit(50).Find(&docs).Error; err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Gagal membaca dokumen"})

	}

	shortcuts := generateDynamicShortcuts(docs, user)
	return c.Status(http.StatusOK).JSON(fiber.Map{"shortcuts": shortcuts})
}
