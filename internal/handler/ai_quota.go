package handler

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"panda-backend/internal/models"
	"panda-backend/internal/service"
)

// GetAIQuota memberi status kuota AI ringkas untuk peringatan di halaman chat (semua user).
func (h *Handler) GetAIQuota(c *gin.Context) {
	cfg := service.LoadLLMConfig(h.DB)
	st := service.GetLLMQuotaStatus(h.DB, cfg, false)
	c.JSON(http.StatusOK, gin.H{"quota": gin.H{
		"engine":          service.EngineLabel(cfg, service.LoadRerankConfig(h.DB)),
		"status":          st.Status,
		"message":         st.Message,
		"percent":         st.Percent,
		"limit":           st.Limit,
		"used_today":      st.UsedToday,
		"remaining":       st.Remaining,
		"resets_at":       st.ResetsAt,
		"exhausted_until": st.ExhaustedUntil,
	}})
}

// GetAIQuotaDetail memberi detail pemakaian kuota AI & rekap 7 hari untuk menu Docs (Super Admin).
func (h *Handler) GetAIQuotaDetail(c *gin.Context) {
	st := service.GetLLMQuotaStatus(h.DB, service.LoadLLMConfig(h.DB), true)
	c.JSON(http.StatusOK, gin.H{"quota": st})
}

// UpdateAIQuotaLimit menyimpan batas permintaan harian manual (0 = ikuti batas dari provider).
func (h *Handler) UpdateAIQuotaLimit(c *gin.Context) {
	actor := getCurrentUser(c)
	var req struct {
		DailyLimit *int `json:"daily_limit"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.DailyLimit == nil || *req.DailyLimit < 0 || *req.DailyLimit > 10_000_000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Batas permintaan harian harus angka 0 atau lebih"})
		return
	}
	if err := service.SaveLLMDailyLimit(h.DB, *req.DailyLimit, actor.BadgeNumber); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Gagal menyimpan batas kuota AI"})
		return
	}

	limitLabel := fmt.Sprintf("%d permintaan/hari", *req.DailyLimit)
	if *req.DailyLimit == 0 {
		limitLabel = "otomatis dari provider"
	}
	h.DB.Create(&models.UploadAuditLog{
		BadgeNumber:        actor.BadgeNumber,
		UserName:           actor.FullName,
		UserRole:           actor.Role,
		TeamAndBranchLabel: formatUserTeamLabel(actor),
		ActionType:         "UPDATE_AI_SETTINGS",
		ModuleSource:       "SYSTEM_SETTINGS",
		DetailDescription:  fmt.Sprintf("%s (%s) mengubah batas kuota Agentic AI: %s", actor.FullName, actor.BadgeNumber, limitLabel),
		ActionTimestamp:    time.Now(),
	})

	c.JSON(http.StatusOK, gin.H{"quota": service.GetLLMQuotaStatus(h.DB, service.LoadLLMConfig(h.DB), true)})
}
