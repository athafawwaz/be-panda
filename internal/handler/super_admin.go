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

// RequireSuperAdmin membatasi endpoint pengembangan (Setting API Key AI) hanya untuk Super Admin.
// Saat impersonate, identitas efektif adalah Pegawai TI sehingga endpoint ini otomatis tertutup.
func (h *Handler) RequireSuperAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if getCurrentUser(c).Role != models.RoleSuperAdmin {
			c.JSON(http.StatusForbidden, gin.H{"error": "Menu ini khusus Super Admin"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// ─── Setting Agentic AI (Provider, Base URL, Model, API Key) ─────────────────

var validProviders = map[string]bool{
	service.ProviderBuiltin: true,
	"openai":                true,
	"gemini":                true,
	"groq":                  true,
	"custom":                true,
}

type aiSettingsRequest struct {
	Provider    string  `json:"provider"`
	BaseURL     string  `json:"base_url"`
	Model       string  `json:"model"`
	APIKey      *string `json:"api_key"`
	ClearAPIKey bool    `json:"clear_api_key"`
	// nil = tidak diubah
	ShareDocContent  *bool `json:"share_doc_content"`
	ChatHistoryTurns *int  `json:"chat_history_turns"`
	ThinkingMaxSteps *int  `json:"thinking_max_steps"`
}

func aiSettingsPayload(cfg service.LLMConfig) gin.H {
	engine := "Built-in Go Smart Document Agent (Zero API Key)"
	toolCalling := false
	if cfg.UsesExternalLLM() {
		engine = "PANDA Agent · tool calling (" + cfg.Model + ")"
		toolCalling = service.ProviderSupportsTools(cfg)
		if !toolCalling {
			engine = "PANDA Thinking · agen JSON (" + cfg.Model + ", provider tanpa tool calling)"
		}
	}
	return gin.H{
		"provider":          cfg.Provider,
		"base_url":          cfg.BaseURL,
		"model":             cfg.Model,
		"has_api_key":       cfg.APIKey != "",
		"api_key_masked":    service.MaskSecret(cfg.APIKey),
		"source":            cfg.Source,
		"engine_mode":       engine,
		"tool_calling":      toolCalling, // provider dipakai lewat agent loop tool calling (false = fallback agen JSON)
		"share_doc_content": cfg.ShareDocContent,
		// Jumlah tanya-jawab terakhir yang dibaca AI saat menjawab (0 = tidak membaca percakapan sebelumnya)
		"chat_history_turns":     cfg.ChatHistoryTurns,
		"max_chat_history_turns": service.MaxChatHistoryTurns,
		// Batas langkah agen riset pada mode Thinking (0 = otomatis, AI berhenti sendiri)
		"thinking_max_steps":     cfg.ThinkingMaxSteps,
		"min_thinking_max_steps": service.MinThinkingMaxSteps,
		"max_thinking_max_steps": service.MaxThinkingMaxSteps,
		"updated_by_badge":       cfg.UpdatedByBadge,
		"updated_at":             cfg.UpdatedAt,
	}
}

func (h *Handler) GetAISettings(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"settings": aiSettingsPayload(service.LoadLLMConfig(h.DB))})
}

func validateAISettings(req aiSettingsRequest) string {
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if !validProviders[provider] {
		return "Provider tidak dikenal"
	}
	if n := req.ChatHistoryTurns; n != nil && (*n < 0 || *n > service.MaxChatHistoryTurns) {
		return fmt.Sprintf("Jumlah riwayat percakapan yang dibaca AI harus 0–%d", service.MaxChatHistoryTurns)
	}
	if n := req.ThinkingMaxSteps; n != nil && *n != service.ThinkingStepsAuto &&
		(*n < service.MinThinkingMaxSteps || *n > service.MaxThinkingMaxSteps) {
		return fmt.Sprintf("Batas langkah Thinking harus otomatis (0) atau %d–%d", service.MinThinkingMaxSteps, service.MaxThinkingMaxSteps)
	}
	if provider == service.ProviderBuiltin {
		return ""
	}
	base := strings.TrimSpace(req.BaseURL)
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
		return "Base URL harus diawali http:// atau https://"
	}
	if strings.TrimSpace(req.Model) == "" {
		return "Model wajib diisi"
	}
	return ""
}

func (h *Handler) UpdateAISettings(c *gin.Context) {
	actor := getCurrentUser(c)
	var req aiSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Format setting AI tidak valid"})
		return
	}
	if msg := validateAISettings(req); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}

	err := service.SaveLLMConfig(h.DB, service.LLMConfigUpdate{
		Provider:         req.Provider,
		BaseURL:          req.BaseURL,
		Model:            req.Model,
		APIKey:           req.APIKey,
		ClearAPIKey:      req.ClearAPIKey,
		ShareDocContent:  req.ShareDocContent,
		ChatHistoryTurns: req.ChatHistoryTurns,
		ThinkingMaxSteps: req.ThinkingMaxSteps,
	}, actor.BadgeNumber)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menyimpan setting AI"})
		return
	}

	keyAction := "API key tidak diubah"
	switch {
	case req.ClearAPIKey:
		keyAction = "API key dihapus"
	case req.APIKey != nil && strings.TrimSpace(*req.APIKey) != "":
		keyAction = "API key diperbarui"
	}
	cfg := service.LoadLLMConfig(h.DB)
	thinkingSteps := "otomatis"
	if cfg.ThinkingMaxSteps > 0 {
		thinkingSteps = fmt.Sprintf("%d keputusan", cfg.ThinkingMaxSteps)
	}
	h.DB.Create(&models.UploadAuditLog{
		BadgeNumber:        actor.BadgeNumber,
		UserName:           actor.FullName,
		UserRole:           actor.Role,
		TeamAndBranchLabel: formatUserTeamLabel(actor),
		ActionType:         "UPDATE_AI_SETTINGS",
		ModuleSource:       "SYSTEM_SETTINGS",
		DetailDescription: fmt.Sprintf("%s (%s) mengubah setting Agentic AI: provider %s, model %s, %s, kirim isi dokumen ke LLM eksternal (upload & chat): %t, riwayat percakapan dibaca AI: %d respons, batas langkah Thinking: %s",
			actor.FullName, actor.BadgeNumber, cfg.Provider, cfg.Model, keyAction, cfg.ShareDocContent, cfg.ChatHistoryTurns, thinkingSteps),
		ActionTimestamp: time.Now(),
	})

	c.JSON(http.StatusOK, gin.H{"settings": aiSettingsPayload(cfg)})
}

// TestAISettings menguji koneksi memakai nilai di form (bila diisi) atau setting tersimpan.
func (h *Handler) TestAISettings(c *gin.Context) {
	var req aiSettingsRequest
	_ = c.ShouldBindJSON(&req)

	cfg := service.LoadLLMConfig(h.DB)
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

	if cfg.Provider == service.ProviderBuiltin {
		c.JSON(http.StatusOK, gin.H{"ok": true, "message": "Mode Built-in Agent aktif — tidak memerlukan koneksi ke LLM eksternal."})
		return
	}
	if msg := validateAISettings(aiSettingsRequest{Provider: cfg.Provider, BaseURL: cfg.BaseURL, Model: cfg.Model}); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	if cfg.APIKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "API key belum diisi"})
		return
	}

	start := time.Now()
	reply, err := service.TestLLMConnection(cfg)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "message": err.Error(), "latency_ms": latency})
		return
	}
	if len(reply) > 200 {
		reply = reply[:200] + "…"
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":         true,
		"message":    fmt.Sprintf("Terhubung ke %s (%s)", cfg.BaseURL, cfg.Model),
		"reply":      reply,
		"latency_ms": latency,
	})
}
