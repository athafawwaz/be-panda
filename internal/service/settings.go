package service

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"panda-backend/internal/models"
)

// Kunci app_settings untuk konfigurasi Agentic AI
const (
	SettingLLMProvider = "LLM_PROVIDER"
	SettingLLMBaseURL  = "LLM_BASE_URL"
	SettingLLMModel    = "LLM_MODEL"
	SettingLLMAPIKey   = "LLM_API_KEY"
	// Satu saklar untuk semua jalur yang mengirim isi dokumen ke LLM eksternal: analisis upload Agentic AI,
	// kutipan isi di chat Agentic AI, dan embedding (bila kelak dipakai). Default: tidak dikirim.
	SettingLLMShareDocContent = "LLM_SHARE_DOC_CONTENT"
	// Jumlah tanya-jawab terakhir di percakapan yang dibaca Agentic AI (0 = AI tidak membaca percakapan sebelumnya)
	SettingLLMChatHistoryTurns = "LLM_CHAT_HISTORY_TURNS"
	// Batas langkah agen saat mode Thinking (setiap langkah = 1 panggilan LLM: cari / baca / simpulkan).
	// 0 = otomatis: agen berhenti sendiri saat bukti cukup (dengan rem pengaman jenuh/waktu/plafon).
	SettingLLMThinkingMaxSteps = "LLM_THINKING_MAX_STEPS"
)

const (
	DefaultChatHistoryTurns = 10
	MaxChatHistoryTurns     = 30
	ThinkingStepsAuto       = 0 // mode Thinking tanpa batas langkah: AI memutuskan sendiri kapan berhenti
	DefaultThinkingMaxSteps = ThinkingStepsAuto
	MinThinkingMaxSteps     = 2
	MaxThinkingMaxSteps     = 40
)

const (
	ProviderBuiltin  = "builtin"
	defaultLLMURL    = "https://api.openai.com/v1"
	defaultLLMModel  = "gpt-4o-mini"
	defaultJWTSecret = "panda-pusri-secret-key-2026"
)

// AppSecret dipakai untuk menandatangani JWT sekaligus menurunkan kunci enkripsi setting secret.
func AppSecret() string {
	if s := os.Getenv("JWT_SECRET"); s != "" {
		return s
	}
	return defaultJWTSecret
}

// LLMConfig adalah konfigurasi efektif: nilai dari database (diatur Super Admin) diprioritaskan,
// lalu fallback ke .env, lalu default.
type LLMConfig struct {
	Provider string
	BaseURL  string
	Model    string
	APIKey   string
	// ShareDocContent: isi dokumen boleh dikirim ke LLM eksternal (analisis upload & kutipan isi di chat Agentic AI);
	// bila mati, LLM hanya menerima metadata dan ringkasan dari pengunggah
	ShareDocContent bool
	// ChatHistoryTurns: jumlah tanya-jawab terakhir di percakapan yang dibaca AI (0–MaxChatHistoryTurns)
	ChatHistoryTurns int
	// ThinkingMaxSteps: batas langkah agen riset pada mode Thinking; ThinkingStepsAuto (0) = tanpa batas,
	// selain itu MinThinkingMaxSteps–MaxThinkingMaxSteps
	ThinkingMaxSteps int
	Source           string // database | env | default
	// Metadata setting tersimpan (kosong bila belum pernah disimpan dari UI)
	UpdatedByBadge string
	UpdatedAt      *time.Time
}

// UsesExternalLLM: LLM eksternal dipakai bila provider bukan builtin dan API key tersedia.
func (c LLMConfig) UsesExternalLLM() bool {
	return c.Provider != ProviderBuiltin && strings.TrimSpace(c.APIKey) != ""
}

func LoadLLMConfig(db *gorm.DB) LLMConfig {
	var rows []models.AppSetting
	db.Where("setting_key IN ?", []string{SettingLLMProvider, SettingLLMBaseURL, SettingLLMModel, SettingLLMAPIKey, SettingLLMShareDocContent, SettingLLMChatHistoryTurns, SettingLLMThinkingMaxSteps}).Find(&rows)

	stored := map[string]models.AppSetting{}
	for _, r := range rows {
		stored[r.SettingKey] = r
	}

	cfg := LLMConfig{Source: "default"}
	if len(stored) > 0 {
		cfg.Source = "database"
	} else if os.Getenv("LLM_API_KEY") != "" || os.Getenv("LLM_PROVIDER") != "" {
		cfg.Source = "env"
	}

	pick := func(key, def string) string {
		if row, ok := stored[key]; ok {
			return row.SettingValue
		}
		return getEnvOrDefault(key, def)
	}

	cfg.Provider = strings.ToLower(strings.TrimSpace(pick(SettingLLMProvider, "")))
	cfg.BaseURL = strings.TrimSpace(pick(SettingLLMBaseURL, defaultLLMURL))
	cfg.Model = strings.TrimSpace(pick(SettingLLMModel, defaultLLMModel))
	cfg.ShareDocContent = strings.EqualFold(strings.TrimSpace(pick(SettingLLMShareDocContent, "false")), "true")
	cfg.ChatHistoryTurns = DefaultChatHistoryTurns
	if n, err := strconv.Atoi(strings.TrimSpace(pick(SettingLLMChatHistoryTurns, strconv.Itoa(DefaultChatHistoryTurns)))); err == nil {
		cfg.ChatHistoryTurns = clampChatHistoryTurns(n)
	}
	cfg.ThinkingMaxSteps = DefaultThinkingMaxSteps
	if n, err := strconv.Atoi(strings.TrimSpace(pick(SettingLLMThinkingMaxSteps, strconv.Itoa(DefaultThinkingMaxSteps)))); err == nil {
		cfg.ThinkingMaxSteps = clampThinkingMaxSteps(n)
	}

	if row, ok := stored[SettingLLMAPIKey]; ok {
		if row.SettingValue != "" {
			key, err := decryptSecret(row.SettingValue)
			if err != nil {
				log.Printf("[Settings] gagal mendekripsi LLM_API_KEY (JWT_SECRET berubah?): %v", err)
			}
			cfg.APIKey = key
		}
	} else {
		cfg.APIKey = strings.TrimSpace(os.Getenv("LLM_API_KEY"))
	}

	// Provider kosong: perilaku lama (otomatis pakai LLM eksternal bila ada API key)
	if cfg.Provider == "" {
		if cfg.APIKey != "" {
			cfg.Provider = "custom"
		} else {
			cfg.Provider = ProviderBuiltin
		}
	}

	for _, r := range stored {
		if cfg.UpdatedAt == nil || r.UpdatedAt.After(*cfg.UpdatedAt) {
			t := r.UpdatedAt
			cfg.UpdatedAt = &t
			cfg.UpdatedByBadge = r.UpdatedByBadge
		}
	}
	return cfg
}

func clampChatHistoryTurns(n int) int {
	return max(0, min(n, MaxChatHistoryTurns))
}

func clampThinkingMaxSteps(n int) int {
	if n <= ThinkingStepsAuto {
		return ThinkingStepsAuto
	}
	return max(MinThinkingMaxSteps, min(n, MaxThinkingMaxSteps))
}

// LLMConfigUpdate: field pointer nil berarti "tidak diubah"; ClearAPIKey menghapus key tersimpan.
type LLMConfigUpdate struct {
	Provider         string
	BaseURL          string
	Model            string
	APIKey           *string
	ClearAPIKey      bool
	ShareDocContent  *bool
	ChatHistoryTurns *int
	ThinkingMaxSteps *int
}

func SaveLLMConfig(db *gorm.DB, upd LLMConfigUpdate, actorBadge string) error {
	current := LoadLLMConfig(db)

	apiKeyValue := ""
	switch {
	case upd.ClearAPIKey:
		apiKeyValue = ""
	case upd.APIKey != nil && strings.TrimSpace(*upd.APIKey) != "":
		enc, err := encryptSecret(strings.TrimSpace(*upd.APIKey))
		if err != nil {
			return err
		}
		apiKeyValue = enc
	case current.APIKey != "":
		// Pertahankan key lama (termasuk yang sebelumnya berasal dari .env)
		enc, err := encryptSecret(current.APIKey)
		if err != nil {
			return err
		}
		apiKeyValue = enc
	}

	share := current.ShareDocContent
	if upd.ShareDocContent != nil {
		share = *upd.ShareDocContent
	}
	historyTurns := current.ChatHistoryTurns
	if upd.ChatHistoryTurns != nil {
		historyTurns = clampChatHistoryTurns(*upd.ChatHistoryTurns)
	}
	thinkingSteps := current.ThinkingMaxSteps
	if upd.ThinkingMaxSteps != nil {
		thinkingSteps = clampThinkingMaxSteps(*upd.ThinkingMaxSteps)
	}

	rows := []models.AppSetting{
		{SettingKey: SettingLLMShareDocContent, SettingValue: strconv.FormatBool(share), UpdatedByBadge: actorBadge},
		{SettingKey: SettingLLMChatHistoryTurns, SettingValue: strconv.Itoa(historyTurns), UpdatedByBadge: actorBadge},
		{SettingKey: SettingLLMThinkingMaxSteps, SettingValue: strconv.Itoa(thinkingSteps), UpdatedByBadge: actorBadge},
		{SettingKey: SettingLLMProvider, SettingValue: strings.ToLower(strings.TrimSpace(upd.Provider)), UpdatedByBadge: actorBadge},
		{SettingKey: SettingLLMBaseURL, SettingValue: strings.TrimRight(strings.TrimSpace(upd.BaseURL), "/"), UpdatedByBadge: actorBadge},
		{SettingKey: SettingLLMModel, SettingValue: strings.TrimSpace(upd.Model), UpdatedByBadge: actorBadge},
		{SettingKey: SettingLLMAPIKey, SettingValue: apiKeyValue, IsSecret: true, UpdatedByBadge: actorBadge},
	}

	return db.Transaction(func(tx *gorm.DB) error {
		for _, r := range rows {
			// Map agar string kosong tetap ditulis (struct Updates melewati zero value)
			res := tx.Model(&models.AppSetting{}).Where("setting_key = ?", r.SettingKey).Updates(map[string]interface{}{
				"setting_value":    r.SettingValue,
				"is_secret":        r.IsSecret,
				"updated_by_badge": r.UpdatedByBadge,
				"updated_at":       time.Now(),
			})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				if err := tx.Create(&r).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// MaskSecret menampilkan sebagian kecil key agar Super Admin bisa mengenali key yang tersimpan.
func MaskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return strings.Repeat("•", len(s))
	}
	return s[:4] + strings.Repeat("•", 8) + s[len(s)-4:]
}

func encryptionKey() []byte {
	sum := sha256.Sum256([]byte("panda-settings:" + AppSecret()))
	return sum[:]
}

func encryptSecret(plain string) (string, error) {
	block, err := aes.NewCipher(encryptionKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func decryptSecret(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(encryptionKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("ciphertext terlalu pendek")
	}
	nonce, data := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, data, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
