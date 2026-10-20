package handler

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"panda-backend/internal/models"
	"panda-backend/internal/service"
)

// ─── Setting Reranker (docs/AGENTIC_AI_PLAN.md §8) ───────────────────────────

type rerankSettingsRequest struct {
	Provider    string  `json:"provider"`
	BaseURL     string  `json:"base_url"`
	Model       string  `json:"model"`
	APIKey      *string `json:"api_key"`
	ClearAPIKey bool    `json:"clear_api_key"`
	Candidates  *int    `json:"candidates"`
	TopN        *int    `json:"top_n"`
}

func rerankSettingsPayload(cfg service.RerankConfig, shareContent bool) gin.H {
	return gin.H{
		"provider":       cfg.Provider,
		"base_url":       cfg.BaseURL,
		"model":          cfg.Model,
		"has_api_key":    cfg.APIKey != "",
		"api_key_masked": service.MaskSecret(cfg.APIKey),
		"enabled":        cfg.Enabled(),
		"candidates":     cfg.Candidates,
		"top_n":          cfg.TopN,
		"min_candidates": service.MinRerankCandidates,
		"max_candidates": service.MaxRerankCandidates,
		"max_top_n":      service.MaxRerankTopN,
		"presets":        service.RerankPresets,
		// Saklar yang sama dengan LLM: isi potongan hanya dikirim ke reranker bila isi dokumen boleh dikirim
		"content_shared":   shareContent,
		"source":           cfg.Source,
		"updated_by_badge": cfg.UpdatedByBadge,
		"updated_at":       cfg.UpdatedAt,
	}
}

func validateRerankSettings(req rerankSettingsRequest) string {
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider == "" || provider == service.RerankProviderNone {
		return ""
	}
	known := false
	for _, p := range service.RerankPresets {
		known = known || p.ID == provider
	}
	if !known {
		return "Provider reranker tidak dikenal"
	}
	base := strings.TrimSpace(req.BaseURL)
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
		return "Base URL reranker harus diawali http:// atau https://"
	}
	if strings.TrimSpace(req.Model) == "" {
		return "Model reranker wajib diisi"
	}
	if n := req.Candidates; n != nil && (*n < service.MinRerankCandidates || *n > service.MaxRerankCandidates) {
		return fmt.Sprintf("Jumlah kandidat harus %d–%d", service.MinRerankCandidates, service.MaxRerankCandidates)
	}
	if n := req.TopN; n != nil && (*n < 1 || *n > service.MaxRerankTopN) {
		return fmt.Sprintf("Top-N harus 1–%d", service.MaxRerankTopN)
	}
	return ""
}

func (h *Handler) GetRerankSettings(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"settings": rerankSettingsPayload(service.LoadRerankConfig(h.DB), service.LoadLLMConfig(h.DB).ShareDocContent)})
}

func (h *Handler) UpdateRerankSettings(c *gin.Context) {
	actor := getCurrentUser(c)
	var req rerankSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Format setting reranker tidak valid"})
		return
	}
	if msg := validateRerankSettings(req); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	if err := service.SaveRerankConfig(h.DB, service.RerankConfigUpdate{
		Provider: req.Provider, BaseURL: req.BaseURL, Model: req.Model, APIKey: req.APIKey, ClearAPIKey: req.ClearAPIKey,
		Candidates: req.Candidates, TopN: req.TopN,
	}, actor.BadgeNumber); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menyimpan setting reranker"})
		return
	}

	keyAction := "API key tidak diubah"
	switch {
	case req.ClearAPIKey:
		keyAction = "API key dihapus"
	case req.APIKey != nil && strings.TrimSpace(*req.APIKey) != "":
		keyAction = "API key diperbarui"
	}
	cfg := service.LoadRerankConfig(h.DB)
	h.DB.Create(&models.UploadAuditLog{
		BadgeNumber:        actor.BadgeNumber,
		UserName:           actor.FullName,
		UserRole:           actor.Role,
		TeamAndBranchLabel: formatUserTeamLabel(actor),
		ActionType:         "UPDATE_RERANK_SETTINGS",
		ModuleSource:       "SYSTEM_SETTINGS",
		DetailDescription: fmt.Sprintf("%s (%s) mengubah setting reranker Agentic AI: provider %s, model %s, %s, kandidat %d, top-N %d, aktif: %t",
			actor.FullName, actor.BadgeNumber, cfg.Provider, cfg.Model, keyAction, cfg.Candidates, cfg.TopN, cfg.Enabled()),
		ActionTimestamp: time.Now(),
	})
	c.JSON(http.StatusOK, gin.H{"settings": rerankSettingsPayload(cfg, service.LoadLLMConfig(h.DB).ShareDocContent)})
}

// TestRerankSettings menguji reranker memakai nilai di form (bila diisi) atau setting tersimpan. Yang dikirim hanya
// kueri & tiga teks contoh, bukan isi dokumen.
func (h *Handler) TestRerankSettings(c *gin.Context) {
	var req rerankSettingsRequest
	_ = c.ShouldBindJSON(&req)
	cfg := service.LoadRerankConfig(h.DB)
	if p := strings.ToLower(strings.TrimSpace(req.Provider)); p != "" {
		cfg.Provider = p
	}
	if b := strings.TrimSpace(req.BaseURL); b != "" {
		cfg.BaseURL = b
	}
	if m := strings.TrimSpace(req.Model); m != "" {
		cfg.Model = m
	}
	if req.APIKey != nil && strings.TrimSpace(*req.APIKey) != "" {
		cfg.APIKey = strings.TrimSpace(*req.APIKey)
	}
	if cfg.Provider == "" || cfg.Provider == service.RerankProviderNone {
		c.JSON(http.StatusOK, gin.H{"ok": true, "message": "Reranker model tidak aktif — pencarian memakai penilaian ulang heuristik."})
		return
	}
	if msg := validateRerankSettings(rerankSettingsRequest{Provider: cfg.Provider, BaseURL: cfg.BaseURL, Model: cfg.Model}); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	if cfg.APIKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "API key reranker belum diisi"})
		return
	}
	scores, docs, latency, err := service.TestReranker(c.Request.Context(), cfg)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "message": err.Error(), "latency_ms": latency.Milliseconds()})
		return
	}
	ranking := make([]gin.H, 0, len(scores))
	for _, s := range scores {
		ranking = append(ranking, gin.H{"text": docs[s.Index], "score": s.Score})
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":         true,
		"message":    fmt.Sprintf("Terhubung ke %s (%s)", cfg.BaseURL, cfg.Model),
		"query":      service.RerankTestQuery(),
		"ranking":    ranking,
		"latency_ms": latency.Milliseconds(),
	})
}
