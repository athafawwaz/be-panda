package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"panda-backend/internal/models"
)

// ─── Reranker: model cross-encoder SaaS (Cohere / Jina / Voyage, atau endpoint kompatibel) ─────────────────────

// RerankScore adalah skor relevansi satu dokumen input (Index = posisinya pada input).
type RerankScore struct {
	Index int
	Score float64
}

// Reranker menilai ulang relevansi teks terhadap kueri.
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []string, topN int) ([]RerankScore, error)
	Name() string // mis. "cohere:rerank-v3.5"
}

const (
	SettingRerankProvider   = "RERANK_PROVIDER"
	SettingRerankBaseURL    = "RERANK_BASE_URL"
	SettingRerankModel      = "RERANK_MODEL"
	SettingRerankAPIKey     = "RERANK_API_KEY"
	SettingRerankCandidates = "RERANK_CANDIDATES"
	SettingRerankTopN       = "RERANK_TOP_N"

	RerankProviderNone      = "none"
	DefaultRerankCandidates = 30
	MinRerankCandidates     = 5
	MaxRerankCandidates     = 50
	DefaultRerankTopN       = 8
	MaxRerankTopN           = 8

	rerankTimeout        = 8 * time.Second
	rerankModelWeight    = 0.75 // porsi skor model pada skor akhir; sisanya heuristik (versi terbaru, metadata, cakupan kata)
	rerankDocTextRunes   = 1500 // isi potongan yang dikirim ke reranker
	rerankSummaryRunes   = 600  // ringkasan pengunggah bila isi tidak boleh dikirim
	rerankTestQuery      = "berita acara serah terima aplikasi"
	rerankWeightsLabel   = "0,75 model + 0,25 heuristik"
	heuristicWeightLabel = "heuristik: RRF, BM25, cakupan & kedekatan kata, metadata, versi terbaru"
)

// RerankPreset adalah alamat & model bawaan per provider (semuanya multibahasa, termasuk Bahasa Indonesia).
type RerankPreset struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
}

var RerankPresets = []RerankPreset{
	{ID: "cohere", Label: "Cohere", BaseURL: "https://api.cohere.com/v2", Model: "rerank-v3.5"},
	{ID: "jina", Label: "Jina AI", BaseURL: "https://api.jina.ai/v1", Model: "jina-reranker-v2-base-multilingual"},
	{ID: "voyage", Label: "Voyage AI", BaseURL: "https://api.voyageai.com/v1", Model: "rerank-2"},
	{ID: "custom", Label: "Custom (Cohere-compatible)", BaseURL: "", Model: ""},
}

func rerankPreset(id string) (RerankPreset, bool) {
	for _, p := range RerankPresets {
		if p.ID == id {
			return p, true
		}
	}
	return RerankPreset{}, false
}

// RerankConfig adalah konfigurasi reranker efektif: database (diatur Super Admin) → .env → default.
type RerankConfig struct {
	Provider       string
	BaseURL        string
	Model          string
	APIKey         string
	Candidates     int
	TopN           int
	Source         string // database | env | default
	UpdatedByBadge string
	UpdatedAt      *time.Time
}

// Enabled: reranker model dipakai bila provider dipilih dan alamat, model, serta API key lengkap.
func (c RerankConfig) Enabled() bool {
	return c.Provider != "" && c.Provider != RerankProviderNone &&
		strings.TrimSpace(c.BaseURL) != "" && strings.TrimSpace(c.Model) != "" && strings.TrimSpace(c.APIKey) != ""
}

func clampRerankCandidates(n int) int { return max(MinRerankCandidates, min(n, MaxRerankCandidates)) }
func clampRerankTopN(n int) int       { return max(1, min(n, MaxRerankTopN)) }

func LoadRerankConfig(db *gorm.DB) RerankConfig {
	cfg := RerankConfig{Source: "default", Candidates: DefaultRerankCandidates, TopN: DefaultRerankTopN}
	stored := map[string]models.AppSetting{}
	if db != nil {
		var rows []models.AppSetting
		db.Where("setting_key IN ?", []string{SettingRerankProvider, SettingRerankBaseURL, SettingRerankModel, SettingRerankAPIKey,
			SettingRerankCandidates, SettingRerankTopN}).Find(&rows)
		for _, r := range rows {
			stored[r.SettingKey] = r
		}
	}
	if len(stored) > 0 {
		cfg.Source = "database"
	} else if os.Getenv(SettingRerankProvider) != "" {
		cfg.Source = "env"
	}
	pick := func(key, def string) string {
		if row, ok := stored[key]; ok {
			return row.SettingValue
		}
		return getEnvOrDefault(key, def)
	}
	cfg.Provider = strings.ToLower(strings.TrimSpace(pick(SettingRerankProvider, RerankProviderNone)))
	preset, _ := rerankPreset(cfg.Provider)
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(pick(SettingRerankBaseURL, preset.BaseURL)), "/")
	cfg.Model = strings.TrimSpace(pick(SettingRerankModel, preset.Model))
	if n, err := strconv.Atoi(strings.TrimSpace(pick(SettingRerankCandidates, strconv.Itoa(DefaultRerankCandidates)))); err == nil {
		cfg.Candidates = clampRerankCandidates(n)
	}
	if n, err := strconv.Atoi(strings.TrimSpace(pick(SettingRerankTopN, strconv.Itoa(DefaultRerankTopN)))); err == nil {
		cfg.TopN = clampRerankTopN(n)
	}
	if row, ok := stored[SettingRerankAPIKey]; ok {
		if row.SettingValue != "" {
			key, err := decryptSecret(row.SettingValue)
			if err != nil {
				log.Printf("[Settings] gagal mendekripsi RERANK_API_KEY (JWT_SECRET berubah?): %v", err)
			}
			cfg.APIKey = key
		}
	} else {
		cfg.APIKey = strings.TrimSpace(os.Getenv(SettingRerankAPIKey))
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

// RerankConfigUpdate: field pointer nil berarti "tidak diubah"; ClearAPIKey menghapus key tersimpan.
type RerankConfigUpdate struct {
	Provider    string
	BaseURL     string
	Model       string
	APIKey      *string
	ClearAPIKey bool
	Candidates  *int
	TopN        *int
}

func SaveRerankConfig(db *gorm.DB, upd RerankConfigUpdate, actorBadge string) error {
	current := LoadRerankConfig(db)
	provider := strings.ToLower(strings.TrimSpace(upd.Provider))
	if provider == "" {
		provider = RerankProviderNone
	}
	if _, ok := rerankPreset(provider); !ok && provider != RerankProviderNone {
		return fmt.Errorf("provider reranker tidak dikenal: %s", provider)
	}

	keyValue := ""
	switch {
	case upd.ClearAPIKey:
	case upd.APIKey != nil && strings.TrimSpace(*upd.APIKey) != "":
		enc, err := encryptSecret(strings.TrimSpace(*upd.APIKey))
		if err != nil {
			return err
		}
		keyValue = enc
	case current.APIKey != "":
		enc, err := encryptSecret(current.APIKey)
		if err != nil {
			return err
		}
		keyValue = enc
	}
	candidates, topN := current.Candidates, current.TopN
	if upd.Candidates != nil {
		candidates = clampRerankCandidates(*upd.Candidates)
	}
	if upd.TopN != nil {
		topN = clampRerankTopN(*upd.TopN)
	}
	rows := []models.AppSetting{
		{SettingKey: SettingRerankProvider, SettingValue: provider, UpdatedByBadge: actorBadge},
		{SettingKey: SettingRerankBaseURL, SettingValue: strings.TrimRight(strings.TrimSpace(upd.BaseURL), "/"), UpdatedByBadge: actorBadge},
		{SettingKey: SettingRerankModel, SettingValue: strings.TrimSpace(upd.Model), UpdatedByBadge: actorBadge},
		{SettingKey: SettingRerankAPIKey, SettingValue: keyValue, IsSecret: true, UpdatedByBadge: actorBadge},
		{SettingKey: SettingRerankCandidates, SettingValue: strconv.Itoa(candidates), UpdatedByBadge: actorBadge},
		{SettingKey: SettingRerankTopN, SettingValue: strconv.Itoa(topN), UpdatedByBadge: actorBadge},
	}
	return upsertSettings(db, rows)
}

// upsertSettings menulis baris app_settings (string kosong tetap ditulis) dalam satu transaksi.
func upsertSettings(db *gorm.DB, rows []models.AppSetting) error {
	return db.Transaction(func(tx *gorm.DB) error {
		for _, r := range rows {
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

// NewReranker membuat reranker model dari konfigurasi; nil bila belum diaktifkan (heuristik dipakai).
func NewReranker(cfg RerankConfig) Reranker {
	if !cfg.Enabled() {
		return nil
	}
	return &httpReranker{provider: cfg.Provider, baseURL: strings.TrimRight(cfg.BaseURL, "/"), model: cfg.Model, apiKey: cfg.APIKey,
		voyage: cfg.Provider == "voyage", timeout: rerankTimeout}
}

// httpReranker: Cohere v2 / Jina v1 / endpoint kompatibel (top_n, results[]) dan Voyage (top_k, data[]).
type httpReranker struct {
	provider, baseURL, model, apiKey string
	voyage                           bool
	timeout                          time.Duration
}

func (r *httpReranker) Name() string { return r.provider + ":" + r.model }

type rerankResponse struct {
	Results []rerankItem `json:"results"`
	Data    []rerankItem `json:"data"`
	Usage   *struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

type rerankItem struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
	Score          float64 `json:"score"`
}

func (r *httpReranker) usageCfg() LLMConfig {
	return LLMConfig{Provider: "rerank:" + r.provider, Model: r.model, APIKey: r.apiKey}
}

func (r *httpReranker) Rerank(ctx context.Context, query string, docs []string, topN int) ([]RerankScore, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	if topN <= 0 || topN > len(docs) {
		topN = len(docs)
	}
	body := map[string]any{"model": r.model, "query": query, "documents": docs}
	if r.voyage {
		body["top_k"] = topN
	} else {
		body["top_n"] = topN
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", r.baseURL+"/rerank", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	resp, err := (&http.Client{Timeout: r.timeout}).Do(req)
	if err != nil {
		recordLLMUsage(r.usageCfg(), 0, 0, 0, err)
		return nil, err
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		llmErr := &LLMError{Status: resp.StatusCode, Body: string(respBytes)}
		recordLLMUsage(r.usageCfg(), resp.StatusCode, 0, 0, llmErr)
		return nil, llmErr
	}
	var parsed rerankResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		recordLLMUsage(r.usageCfg(), resp.StatusCode, 0, 0, nil)
		return nil, fmt.Errorf("respons reranker tidak valid")
	}
	items := parsed.Results
	if len(items) == 0 {
		items = parsed.Data
	}
	tokens := 0
	if parsed.Usage != nil {
		tokens = parsed.Usage.TotalTokens
	}
	recordLLMUsage(r.usageCfg(), resp.StatusCode, tokens, 0, nil)

	out := make([]RerankScore, 0, len(items))
	seen := map[int]bool{}
	for _, it := range items {
		if it.Index < 0 || it.Index >= len(docs) || seen[it.Index] {
			continue
		}
		seen[it.Index] = true
		score := it.RelevanceScore
		if score == 0 && it.Score != 0 {
			score = it.Score
		}
		out = append(out, RerankScore{Index: it.Index, Score: score})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("reranker tidak mengembalikan hasil")
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

// normalizeRerankScores mengubah skor model menjadi 0–1 (min-max) agar bisa digabung dengan skor heuristik.
// Dokumen yang tidak dikembalikan reranker (di luar top-N) mendapat 0.
func normalizeRerankScores(scores []RerankScore, n int) []float64 {
	out := make([]float64, n)
	if len(scores) == 0 {
		return out
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, s := range scores {
		lo, hi = math.Min(lo, s.Score), math.Max(hi, s.Score)
	}
	for _, s := range scores {
		v := 1.0
		if hi > lo {
			v = (s.Score - lo) / (hi - lo)
		}
		// Hasil terbawah top-N tetap di atas dokumen yang tidak dikembalikan sama sekali
		out[s.Index] = 0.05 + 0.95*v
	}
	return out
}

// rerankText menyusun teks kandidat untuk reranker: header kontekstual (judul, kategori, aplikasi, versi) + isi
// potongan. Bila isi dokumen tidak boleh dikirim, hanya header + ringkasan pengunggah (setara data yang sudah boleh
// dikirim ke LLM).
func rerankText(ev *evidence, shareContent bool) string {
	d := ev.Doc
	app := ""
	if d.Application != nil {
		app = fmt.Sprintf(" | Aplikasi: %s (%s)", d.Application.AppName, d.Application.AppCode)
	}
	header := fmt.Sprintf("Judul: %s | Kategori: %s%s | Versi: %s", d.Title, d.DocCategory, app, d.VersionLabel)
	if shareContent {
		return header + "\n" + clip(oneLine(ev.Text), rerankDocTextRunes)
	}
	return header + "\nRingkasan: " + clip(oneLine(d.AISummary), rerankSummaryRunes)
}

// TestReranker mengirim kueri contoh dengan tiga teks buatan (bukan isi dokumen) untuk memastikan konfigurasi valid.
func TestReranker(ctx context.Context, cfg RerankConfig) ([]RerankScore, []string, time.Duration, error) {
	r := NewReranker(cfg)
	if r == nil {
		return nil, nil, 0, fmt.Errorf("reranker belum lengkap: pilih provider, isi base URL, model, dan API key")
	}
	docs := []string{
		"Prosedur pencadangan data harian ke server cadangan.",
		"Berita acara serah terima aplikasi CorePay versi 1.1 kepada Tim Keuangan.",
		"Laporan hasil uji penetrasi aplikasi portal karyawan.",
	}
	start := time.Now()
	scores, err := r.Rerank(ctx, rerankTestQuery, docs, len(docs))
	return scores, docs, time.Since(start), err
}

// RerankTestQuery dipakai panel pengaturan untuk menampilkan kueri contoh.
func RerankTestQuery() string { return rerankTestQuery }
