package service

import (
	"context"
	"math"
	"strings"
	"time"

	"gorm.io/gorm"

	"panda-backend/internal/models"
)

// Evaluasi reranker (docs/AGENTIC_AI_PLAN.md §11 #5): pipeline tool search_documents dijalankan dua kali untuk
// kandidat yang sama — heuristik saja vs reranker model — lalu dibandingkan dengan MRR, Hit@3, dan nDCG@5.

const rerankEvalK = 5

// RerankMetrics adalah metrik kualitas urutan dokumen.
type RerankMetrics struct {
	MRR     float64 `json:"mrr"`
	HitAt3  float64 `json:"hit_at_3"` // dokumen benar ada di 3 teratas
	NDCGAt5 float64 `json:"ndcg_at_5"`
}

// RerankEvalCase adalah hasil satu pertanyaan uji.
type RerankEvalCase struct {
	ID             string   `json:"id"`
	Question       string   `json:"question"`
	Candidates     int      `json:"candidates"`
	HeuristicRank  int      `json:"heuristic_rank"` // peringkat dokumen benar pertama (0 = tidak masuk top-N)
	ModelRank      int      `json:"model_rank"`
	HeuristicTop   []string `json:"heuristic_top"`
	ModelTop       []string `json:"model_top"`
	ModelLatencyMs int64    `json:"model_latency_ms"`
	Fallback       string   `json:"fallback,omitempty"` // reranker model gagal pada kasus ini
}

// RerankEvalReport adalah perbandingan heuristik vs reranker model.
type RerankEvalReport struct {
	Reranker      string           `json:"reranker"`
	ContentShared bool             `json:"content_shared"`
	Cases         int              `json:"cases"`
	Heuristic     RerankMetrics    `json:"heuristic"`
	Model         RerankMetrics    `json:"model"`
	AvgLatencyMs  float64          `json:"avg_latency_ms"`
	Results       []RerankEvalCase `json:"results"`
}

// docOrder: urutan dokumen unik (standardized_name) dari temuan yang sudah diurutkan.
func docOrder(ranked []*evidence) []string {
	var out []string
	seen := map[string]bool{}
	for _, ev := range ranked {
		name := strings.ToLower(ev.Doc.StandardizedName)
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func firstRelevantRank(order []string, expected map[string]bool) int {
	for i, name := range order {
		if expected[name] {
			return i + 1
		}
	}
	return 0
}

// ndcgAt: nDCG dengan relevansi biner (dokumen yang diharapkan = 1).
func ndcgAt(order []string, expected map[string]bool, k int) float64 {
	dcg := 0.0
	for i := 0; i < len(order) && i < k; i++ {
		if expected[order[i]] {
			dcg += 1 / math.Log2(float64(i+2))
		}
	}
	idcg := 0.0
	for i := 0; i < len(expected) && i < k; i++ {
		idcg += 1 / math.Log2(float64(i+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

func summarizeRerank(ranks []int, ndcgs []float64) RerankMetrics {
	if len(ranks) == 0 {
		return RerankMetrics{}
	}
	var rr, hit, nd float64
	for i, r := range ranks {
		if r > 0 {
			rr += 1 / float64(r)
		}
		if r > 0 && r <= 3 {
			hit++
		}
		nd += ndcgs[i]
	}
	n := float64(len(ranks))
	return RerankMetrics{MRR: round3(rr / n), HitAt3: round3(hit / n), NDCGAt5: round3(nd / n)}
}

// RunRerankEval membandingkan heuristik vs reranker model pada pertanyaan uji positif (yang punya expect_docs).
// Isi potongan hanya dikirim ke reranker bila saklar isi dokumen menyala (sama seperti saat chat).
func RunRerankEval(ctx context.Context, db *gorm.DB, user models.User, cases []EvalCase, rcfg RerankConfig, shareContent bool) RerankEvalReport {
	model := NewReranker(rcfg)
	report := RerankEvalReport{ContentShared: shareContent}
	if model != nil {
		report.Reranker = model.Name()
	}
	var hRanks, mRanks []int
	var hNDCG, mNDCG []float64
	var latency int64
	for _, c := range cases {
		if c.Negative || len(c.ExpectDocs) == 0 {
			continue
		}
		expected := map[string]bool{}
		for _, name := range c.ExpectDocs {
			expected[strings.ToLower(name)] = true
		}
		req := AIChatRequest{Prompt: c.Question, FilterModule: c.FilterModule}
		a, err := newAgentRun(db, user, req, LLMConfig{Provider: ProviderBuiltin, ShareDocContent: shareContent}, newTracer(false, nil))
		if err != nil {
			continue
		}
		a.rerankCfg = rcfg
		cands, _ := a.searchFiltered(c.Question, docFilter{}, max(rcfg.Candidates, MinRerankCandidates))
		st := a.tr.begin("tool", "eval")
		st.Tool = &ToolCallInfo{Name: "search_documents"}
		topN := max(rcfg.TopN, rerankEvalK)

		a.reranker = nil
		heur, _, _ := a.rerankCandidates(ctx, c.Question, cands, topN, st)
		res := RerankEvalCase{ID: c.ID, Question: c.Question, Candidates: len(cands), HeuristicTop: docOrder(heur)}
		a.reranker = model
		start := time.Now()
		ranked, _, info := a.rerankCandidates(ctx, c.Question, cands, topN, st)
		res.ModelLatencyMs = time.Since(start).Milliseconds()
		res.ModelTop = docOrder(ranked)
		res.Fallback = info.Fallback
		res.HeuristicRank = firstRelevantRank(res.HeuristicTop, expected)
		res.ModelRank = firstRelevantRank(res.ModelTop, expected)

		hRanks, mRanks = append(hRanks, res.HeuristicRank), append(mRanks, res.ModelRank)
		hNDCG, mNDCG = append(hNDCG, ndcgAt(res.HeuristicTop, expected, rerankEvalK)), append(mNDCG, ndcgAt(res.ModelTop, expected, rerankEvalK))
		latency += res.ModelLatencyMs
		report.Results = append(report.Results, res)
	}
	report.Cases = len(report.Results)
	report.Heuristic = summarizeRerank(hRanks, hNDCG)
	report.Model = summarizeRerank(mRanks, mNDCG)
	if report.Cases > 0 {
		report.AvgLatencyMs = math.Round(float64(latency) / float64(report.Cases))
	}
	return report
}
