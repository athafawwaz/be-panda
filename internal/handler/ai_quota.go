package handler

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v3"

	"panda-backend/internal/models"
	"panda-backend/internal/service"
)

// GetAIQuota memberi status kuota AI ringkas untuk peringatan di halaman chat (semua user).
func (h *Handler) GetAIQuota(c fiber.Ctx) error {
	cfg := service.LoadLLMConfig(h.DB)
	st := service.GetLLMQuotaStatus(h.DB, cfg, false)
	return c.Status(http.StatusOK).JSON(fiber.Map{"quota": fiber.Map{
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
func (h *Handler) GetAIQuotaDetail(c fiber.Ctx) error {
	st := service.GetLLMQuotaStatus(h.DB, service.LoadLLMConfig(h.DB), true)
	return c.Status(http.StatusOK).JSON(fiber.Map{"quota": st})
}

// UpdateAIQuotaLimit menyimpan batas permintaan harian manual (0 = ikuti batas dari provider).
func (h *Handler) UpdateAIQuotaLimit(c fiber.Ctx) error {
	actor := getCurrentUser(c)
	var req struct {
		DailyLimit *int `json:"daily_limit"`
	}
	if err := c.Bind().JSON(&req); err != nil || req.DailyLimit == nil || *req.DailyLimit < 0 || *req.DailyLimit > 10_000_000 {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "Batas permintaan harian harus angka 0 atau lebih"})

	}
	if err := service.SaveLLMDailyLimit(h.DB, *req.DailyLimit, actor.BadgeNumber); err != nil {
		return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "Gagal menyimpan batas kuota AI"})

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

	return c.Status(http.StatusOK).JSON(fiber.Map{"quota": service.GetLLMQuotaStatus(h.DB, service.LoadLLMConfig(h.DB), true)})
}
