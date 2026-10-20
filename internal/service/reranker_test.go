package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func rerankServer(t *testing.T, handler func(w http.ResponseWriter, path string, body map[string]any)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if r.Header.Get("Authorization") != "Bearer rk" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler(w, r.URL.Path, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestCohereStyleReranker(t *testing.T) {
	var sent map[string]any
	url := rerankServer(t, func(w http.ResponseWriter, path string, body map[string]any) {
		sent = body
		if path != "/rerank" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"results":[{"index":2,"relevance_score":0.91},{"index":0,"relevance_score":0.40},{"index":7,"relevance_score":0.9}]}`)
	})
	r := NewReranker(RerankConfig{Provider: "jina", BaseURL: url + "/", Model: "jina-reranker-v2-base-multilingual", APIKey: "rk"})
	scores, err := r.Rerank(context.Background(), "serah terima", []string{"a", "b", "c"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if sent["top_n"].(float64) != 2 || sent["query"] != "serah terima" || len(sent["documents"].([]any)) != 3 {
		t.Fatalf("payload Cohere/Jina salah: %v", sent)
	}
	if len(scores) != 2 || scores[0].Index != 2 || scores[1].Index != 0 {
		t.Fatalf("skor harus urut & index di luar rentang dibuang: %+v", scores)
	}
	if r.Name() != "jina:jina-reranker-v2-base-multilingual" {
		t.Fatalf("nama reranker salah: %s", r.Name())
	}
}

func TestVoyageRerankerAndErrors(t *testing.T) {
	var sent map[string]any
	url := rerankServer(t, func(w http.ResponseWriter, path string, body map[string]any) {
		sent = body
		fmt.Fprint(w, `{"data":[{"index":1,"relevance_score":0.8},{"index":0,"relevance_score":0.2}],"usage":{"total_tokens":42}}`)
	})
	r := NewReranker(RerankConfig{Provider: "voyage", BaseURL: url, Model: "rerank-2", APIKey: "rk"})
	scores, err := r.Rerank(context.Background(), "q", []string{"a", "b"}, 5)
	if err != nil || len(scores) != 2 || scores[0].Index != 1 {
		t.Fatalf("respons Voyage salah: %+v %v", scores, err)
	}
	if sent["top_k"].(float64) != 2 || sent["top_n"] != nil {
		t.Fatalf("Voyage memakai top_k (dibatasi jumlah dokumen): %v", sent)
	}

	bad := NewReranker(RerankConfig{Provider: "cohere", BaseURL: url, Model: "rerank-v3.5", APIKey: "salah"})
	_, err = bad.Rerank(context.Background(), "q", []string{"a"}, 1)
	var llmErr *LLMError
	if !errors.As(err, &llmErr) || llmErr.Status != http.StatusUnauthorized || describeRerankFailure(err) != "API key reranker ditolak" {
		t.Fatalf("401 harus dilaporkan sebagai key ditolak: %v", err)
	}

	empty := rerankServer(t, func(w http.ResponseWriter, path string, body map[string]any) { fmt.Fprint(w, `{"results":[]}`) })
	if _, err := NewReranker(RerankConfig{Provider: "cohere", BaseURL: empty, Model: "m", APIKey: "rk"}).Rerank(context.Background(), "q", []string{"a"}, 1); err == nil {
		t.Fatalf("respons tanpa hasil harus dianggap gagal")
	}
	if NewReranker(RerankConfig{Provider: "cohere", BaseURL: url, Model: "m"}) != nil {
		t.Fatalf("tanpa API key reranker model tidak boleh aktif")
	}
}

func TestNormalizeRerankScores(t *testing.T) {
	norm := normalizeRerankScores([]RerankScore{{Index: 0, Score: 0.9}, {Index: 2, Score: 0.1}}, 3)
	if norm[0] != 1 || norm[1] != 0 || norm[2] != 0.05 {
		t.Fatalf("normalisasi salah: %v", norm)
	}
	if same := normalizeRerankScores([]RerankScore{{Index: 1, Score: 0.3}}, 2); same[1] != 1 {
		t.Fatalf("satu hasil harus bernilai 1: %v", same)
	}
}

type fakeReranker struct {
	order []int // index kandidat dari paling relevan
	err   error
	texts []string
}

func (f *fakeReranker) Name() string { return "fake:model-x" }

func (f *fakeReranker) Rerank(ctx context.Context, query string, docs []string, topN int) ([]RerankScore, error) {
	f.texts = docs
	if f.err != nil {
		return nil, f.err
	}
	out := make([]RerankScore, len(f.order))
	for i, idx := range f.order {
		out[i] = RerankScore{Index: idx, Score: 1 - float64(i)*0.3}
	}
	return out, nil
}

func rerankTestRun(share bool) (*agentRun, []*evidence, *AgentStep) {
	a := newTestToolAgent(LLMConfig{Model: "m", ShareDocContent: share}, "hasil uji", false, nil)
	uat, sop := a.docByID["doc-uat"], a.docByID["doc-sop"]
	cands := []*evidence{
		{Ref: "E1", Doc: sop, ChunkID: "c1", Text: "hasil uji prosedur backup hasil uji", BestBM25: 3, RRF: 0.02},
		{Ref: "E2", Doc: uat, ChunkID: "c2", Text: "langkah pengujian modul tiket tamu", BestBM25: 1, RRF: 0.01},
	}
	st := a.tr.begin("tool", "Mencari dokumen")
	st.Tool = &ToolCallInfo{Name: "search_documents"}
	return a, cands, st
}

func TestRerankCandidatesUsesModelScores(t *testing.T) {
	a, cands, st := rerankTestRun(true)
	fake := &fakeReranker{order: []int{1, 0}}
	a.reranker = fake
	ranked, scores, info := a.rerankCandidates(context.Background(), "hasil uji", cands, 8, st)
	if info.Method != "model" || info.Provider != "fake" || info.Model != "model-x" || info.Weights != rerankWeightsLabel || !info.ContentShared {
		t.Fatalf("info rerank salah: %+v", info)
	}
	if ranked[0].Ref != "E2" || scores[ranked[0]] <= scores[ranked[1]] {
		t.Fatalf("skor model (75%%) harus mengalahkan heuristik: %v %v", ranked[0].Ref, scores)
	}
	if !strings.Contains(fake.texts[1], "Judul: Laporan UAT VISTA | Kategori: UAT | Aplikasi: Visitor is The Best (VISTA) | Versi: v1.1") ||
		!strings.Contains(fake.texts[1], "langkah pengujian modul tiket tamu") {
		t.Fatalf("teks reranker harus memuat header kontekstual & isi: %q", fake.texts[1])
	}
}

func TestRerankCandidatesFallbackAndContentPolicy(t *testing.T) {
	a, cands, st := rerankTestRun(false)
	fake := &fakeReranker{err: fmt.Errorf("koneksi putus")}
	a.reranker = fake
	ranked, _, info := a.rerankCandidates(context.Background(), "hasil uji", cands, 8, st)
	if info.Method != "heuristic" || info.Fallback == "" || ranked[0].Ref != "E1" {
		t.Fatalf("reranker gagal harus jatuh ke heuristik: %+v first=%s", info, ranked[0].Ref)
	}
	for _, txt := range fake.texts {
		if strings.Contains(txt, "modul tiket tamu") || strings.Contains(txt, "prosedur backup") || !strings.Contains(txt, "Ringkasan:") {
			t.Fatalf("saklar isi dokumen mati: hanya header + ringkasan yang boleh dikirim: %q", txt)
		}
	}

	a, cands, st = rerankTestRun(true)
	a.rerankCfg.Provider = "cohere" // dipilih tapi API key kosong
	_, _, info = a.rerankCandidates(context.Background(), "hasil uji", cands, 1, st)
	if info.Method != "heuristic" || !strings.Contains(info.Fallback, "belum lengkap") || info.Returned != 1 {
		t.Fatalf("konfigurasi tidak lengkap harus dijelaskan: %+v", info)
	}
}

func TestLoadRerankConfigFromEnv(t *testing.T) {
	t.Setenv(SettingRerankProvider, "jina")
	t.Setenv(SettingRerankAPIKey, "rk")
	t.Setenv(SettingRerankCandidates, "500")
	t.Setenv(SettingRerankTopN, "0")
	cfg := LoadRerankConfig(nil)
	if !cfg.Enabled() || cfg.BaseURL != "https://api.jina.ai/v1" || cfg.Model != "jina-reranker-v2-base-multilingual" || cfg.Source != "env" {
		t.Fatalf("preset Jina dari .env salah: %+v", cfg)
	}
	if cfg.Candidates != MaxRerankCandidates || cfg.TopN != 1 {
		t.Fatalf("batas kandidat/top-N harus di-clamp: %+v", cfg)
	}
	t.Setenv(SettingRerankProvider, "")
	if LoadRerankConfig(nil).Enabled() {
		t.Fatalf("tanpa provider reranker model tidak aktif")
	}
}
