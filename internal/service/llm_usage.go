package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"panda-backend/internal/models"
)

// Hari kuota LLM dihitung mulai 00.00 UTC (07.00 WIB): kuota harian Gemini terbukti pulih tepat
// pukul 00.00 UTC (RetryInfo pada error 429). Provider tidak menyediakan API sisa kuota, sehingga
// pemakaian dihitung dari permintaan PANDA sendiri.
const (
	SettingLLMDailyLimit = "LLM_DAILY_REQUEST_LIMIT"
	quotaLowPercent      = 80
	quotaHistoryDays     = 7
)

// Status kuota LLM
const (
	QuotaBuiltin   = "BUILTIN"   // tidak memakai LLM eksternal
	QuotaOK        = "OK"        // masih aman
	QuotaLow       = "LOW"       // sudah terpakai ≥ quotaLowPercent
	QuotaExhausted = "EXHAUSTED" // habis
	QuotaUnknown   = "UNKNOWN"   // batas harian belum diketahui
)

var wib = time.FixedZone("WIB", 7*3600)

var usageDB *gorm.DB

// EnableLLMUsageTracking mengaktifkan pencatatan setiap permintaan LLM ke tabel llm_usage_logs.
func EnableLLMUsageTracking(db *gorm.DB) { usageDB = db }

// ErrQuotaExhausted: permintaan LLM tidak dikirim karena provider sudah menyatakan kuota habis.
type ErrQuotaExhausted struct{ Until time.Time }

func (e *ErrQuotaExhausted) Error() string {
	return "kuota LLM habis sampai " + e.Until.Format(time.RFC3339)
}

func keyFingerprint(apiKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(apiKey)))
	return hex.EncodeToString(sum[:8])
}

var (
	retryInPattern    = regexp.MustCompile(`retry in (\d[\dhm]*(?:\.\d+)?s)`)
	retryDelayPattern = regexp.MustCompile(`"retryDelay":\s*"(\d+(?:\.\d+)?s)"`)
)

// parseRetryDelay membaca lama tunggu sampai kuota pulih dari isi error 429.
func parseRetryDelay(body string) (time.Duration, bool) {
	for _, p := range []*regexp.Regexp{retryInPattern, retryDelayPattern} {
		if m := p.FindStringSubmatch(body); m != nil {
			if d, err := time.ParseDuration(m[1]); err == nil {
				return d, true
			}
		}
	}
	return 0, false
}

// parseDailyQuotaLimit mencari batas permintaan harian ("quotaValue" pada quotaId "...PerDay...") di error 429.
func parseDailyQuotaLimit(body string) (int, bool) {
	var root interface{}
	if json.Unmarshal([]byte(body), &root) != nil {
		return 0, false
	}
	limit, found := 0, false
	var walk func(v interface{})
	walk = func(v interface{}) {
		switch t := v.(type) {
		case map[string]interface{}:
			if id, ok := t["quotaId"].(string); ok && strings.Contains(id, "PerDay") {
				if s, ok := t["quotaValue"].(string); ok {
					if n, err := strconv.Atoi(s); err == nil {
						limit, found = n, true
					}
				}
			}
			for _, x := range t {
				walk(x)
			}
		case []interface{}:
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(root)
	return limit, found
}

// recordLLMUsage mencatat satu permintaan ke LLM eksternal (berhasil atau gagal).
func recordLLMUsage(cfg LLMConfig, statusCode int, promptTokens, completionTokens int, callErr error) {
	if usageDB == nil {
		return
	}
	now := time.Now().UTC()
	row := models.LLMUsageLog{
		Provider:         cfg.Provider,
		Model:            cfg.Model,
		KeyFingerprint:   keyFingerprint(cfg.APIKey),
		StatusCode:       statusCode,
		Success:          callErr == nil,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		CreatedAt:        now,
	}
	if callErr != nil {
		row.ErrorMessage = truncateText(describeLLMFailure(callErr), 480)
		var llmErr *LLMError
		if errors.As(callErr, &llmErr) && llmErr.Status == http.StatusTooManyRequests {
			retry := now.Add(time.Minute)
			if d, ok := parseRetryDelay(llmErr.Body); ok {
				retry = now.Add(d)
			}
			row.RetryUntil = &retry
			if limit, ok := parseDailyQuotaLimit(llmErr.Body); ok {
				row.QuotaLimit = &limit
			}
		}
	}
	if err := usageDB.Create(&row).Error; err != nil {
		log.Printf("[LLM Usage] gagal mencatat pemakaian LLM: %v", err)
	}
}

func loadDailyLimitSetting(db *gorm.DB) int {
	var s models.AppSetting
	if db.Where("setting_key = ?", SettingLLMDailyLimit).Limit(1).Find(&s).RowsAffected == 0 {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(s.SettingValue))
	return n
}

// SaveLLMDailyLimit menyimpan batas permintaan harian manual (0 = ikuti batas yang disebut provider).
func SaveLLMDailyLimit(db *gorm.DB, limit int, actorBadge string) error {
	values := map[string]interface{}{
		"setting_value":    strconv.Itoa(limit),
		"is_secret":        false,
		"updated_by_badge": actorBadge,
		"updated_at":       time.Now(),
	}
	res := db.Model(&models.AppSetting{}).Where("setting_key = ?", SettingLLMDailyLimit).Updates(values)
	if res.Error != nil || res.RowsAffected > 0 {
		return res.Error
	}
	return db.Create(&models.AppSetting{SettingKey: SettingLLMDailyLimit, SettingValue: strconv.Itoa(limit), UpdatedByBadge: actorBadge}).Error
}

type LLMUsageDay struct {
	Day     string `json:"day"` // tanggal UTC (hari kuota dimulai 07.00 WIB)
	Success int    `json:"success"`
	Failed  int    `json:"failed"`
	Tokens  int    `json:"tokens"`
}

// LLMQuotaStatus: perkiraan pemakaian kuota LLM hari ini untuk model & API key yang aktif.
type LLMQuotaStatus struct {
	Status         string        `json:"status"`
	Message        string        `json:"message"`
	Provider       string        `json:"provider"`
	Model          string        `json:"model"`
	UsedToday      int           `json:"used_today"`   // permintaan berhasil sejak reset terakhir
	FailedToday    int           `json:"failed_today"` // permintaan gagal (tidak memakai kuota)
	TokensToday    int           `json:"tokens_today"`
	Limit          int           `json:"limit"`        // 0 = belum diketahui
	LimitSource    string        `json:"limit_source"` // SETTING | PROVIDER | ""
	Remaining      int           `json:"remaining"`
	Percent        float64       `json:"percent"`
	ResetsAt       time.Time     `json:"resets_at"`
	ExhaustedUntil *time.Time    `json:"exhausted_until"`
	LastError      string        `json:"last_error"`
	LastErrorAt    *time.Time    `json:"last_error_at"`
	Daily          []LLMUsageDay `json:"daily,omitempty"`
}

var idMonths = [...]string{"Jan", "Feb", "Mar", "Apr", "Mei", "Jun", "Jul", "Agu", "Sep", "Okt", "Nov", "Des"}

// formatWIB: "17 Okt 07.00 WIB"
func formatWIB(t time.Time) string {
	t = t.In(wib)
	return fmt.Sprintf("%d %s %s WIB", t.Day(), idMonths[t.Month()-1], t.Format("15.04"))
}

// GetLLMQuotaStatus menghitung status kuota LLM hari ini. withHistory menambahkan rekap 7 hari terakhir.
func GetLLMQuotaStatus(db *gorm.DB, cfg LLMConfig, withHistory bool) LLMQuotaStatus {
	st := LLMQuotaStatus{Status: QuotaBuiltin, Provider: cfg.Provider, Model: cfg.Model}
	now := time.Now().UTC()
	windowStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	st.ResetsAt = windowStart.Add(24 * time.Hour)
	if !cfg.UsesExternalLLM() {
		return st
	}

	fp := keyFingerprint(cfg.APIKey)
	scope := func() *gorm.DB {
		return db.Model(&models.LLMUsageLog{}).Where("key_fingerprint = ? AND model = ?", fp, cfg.Model)
	}

	var agg struct{ Success, Failed, Tokens int }
	scope().Select("COALESCE(SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END), 0) AS success, " +
		"COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0) AS failed, " +
		"COALESCE(SUM(prompt_tokens + completion_tokens), 0) AS tokens").
		Where("created_at >= ?", windowStart).Scan(&agg)
	st.UsedToday, st.FailedToday, st.TokensToday = agg.Success, agg.Failed, agg.Tokens

	// Batas harian: diatur manual oleh Super Admin, atau dipelajari dari error 429 provider
	if n := loadDailyLimitSetting(db); n > 0 {
		st.Limit, st.LimitSource = n, "SETTING"
	} else {
		var learned models.LLMUsageLog
		if scope().Where("quota_limit IS NOT NULL").Order("id DESC").Limit(1).Find(&learned).RowsAffected > 0 {
			st.Limit, st.LimitSource = *learned.QuotaLimit, "PROVIDER"
		}
	}

	// Provider menyatakan kuota habis: error 429 terakhir yang masa tunggunya belum lewat
	// dan belum ada permintaan berhasil setelahnya
	var last429 models.LLMUsageLog
	if scope().Where("status_code = ?", http.StatusTooManyRequests).Order("id DESC").Limit(1).Find(&last429).RowsAffected > 0 &&
		last429.RetryUntil != nil && last429.RetryUntil.After(now) {
		var successAfter int64
		scope().Where("success = 1 AND id > ?", last429.ID).Count(&successAfter)
		if successAfter == 0 {
			st.ExhaustedUntil = last429.RetryUntil
		}
	}

	var lastErr models.LLMUsageLog
	if scope().Where("success = 0").Order("id DESC").Limit(1).Find(&lastErr).RowsAffected > 0 {
		at := lastErr.CreatedAt
		st.LastError, st.LastErrorAt = lastErr.ErrorMessage, &at
	}

	if st.Limit > 0 {
		st.Remaining = int(math.Max(0, float64(st.Limit-st.UsedToday)))
		st.Percent = math.Min(100, math.Round(float64(st.UsedToday)*1000/float64(st.Limit))/10)
	}

	switch {
	case st.ExhaustedUntil != nil:
		st.Status, st.Percent, st.Remaining = QuotaExhausted, 100, 0
		st.Message = fmt.Sprintf("Kuota AI sudah habis. Penjelasan AI tidak tersedia sampai sekitar %s (%s lagi); pencarian dokumen tetap berjalan.",
			formatWIB(*st.ExhaustedUntil), humanizeDuration(st.ExhaustedUntil.Sub(now)))
	case st.Limit > 0 && st.UsedToday >= st.Limit:
		st.Status = QuotaExhausted
		st.Message = fmt.Sprintf("Kuota AI hari ini sudah terpakai semua (%d dari %d permintaan) dan pulih %s.",
			st.UsedToday, st.Limit, formatWIB(st.ResetsAt))
	case st.Limit > 0 && st.Percent >= quotaLowPercent:
		st.Status = QuotaLow
		st.Message = fmt.Sprintf("Kuota AI hampir habis: tersisa %d dari %d permintaan hari ini (pulih %s).",
			st.Remaining, st.Limit, formatWIB(st.ResetsAt))
	case st.Limit > 0:
		st.Status = QuotaOK
	default:
		st.Status = QuotaUnknown
	}

	if withHistory {
		st.Daily = loadUsageHistory(scope, windowStart)
	}
	return st
}

// loadUsageHistory merekap pemakaian per hari kuota (UTC) selama quotaHistoryDays terakhir.
func loadUsageHistory(scope func() *gorm.DB, windowStart time.Time) []LLMUsageDay {
	from := windowStart.AddDate(0, 0, -(quotaHistoryDays - 1))
	var rows []struct {
		Day                     time.Time
		Success, Failed, Tokens int
	}
	scope().Select("CAST(created_at AS DATE) AS day, " +
		"SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END) AS success, " +
		"SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END) AS failed, " +
		"COALESCE(SUM(prompt_tokens + completion_tokens), 0) AS tokens").
		Where("created_at >= ?", from).
		Group("CAST(created_at AS DATE)").
		Scan(&rows)

	byDay := map[string]LLMUsageDay{}
	for _, r := range rows {
		key := r.Day.Format("2006-01-02")
		byDay[key] = LLMUsageDay{Day: key, Success: r.Success, Failed: r.Failed, Tokens: r.Tokens}
	}
	days := make([]LLMUsageDay, 0, quotaHistoryDays)
	for i := 0; i < quotaHistoryDays; i++ {
		key := windowStart.AddDate(0, 0, -i).Format("2006-01-02")
		d, ok := byDay[key]
		if !ok {
			d = LLMUsageDay{Day: key}
		}
		days = append(days, d)
	}
	return days
}
