package service

import (
	"math"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"panda-backend/internal/models"
)

// EvalCase adalah satu pertanyaan uji retrieval (lihat backend/eval/README.md).
type EvalCase struct {
	ID            string   `json:"id"`
	Question      string   `json:"question"`
	History       []string `json:"history,omitempty"`        // pertanyaan sebelumnya di percakapan yang sama, dijalankan berurutan
	ExpectDocs    []string `json:"expect_docs,omitempty"`    // standardized_name dokumen yang benar
	ExpectPhrases []string `json:"expect_phrases,omitempty"` // frasa yang harus ada di potongan konteks dokumen yang benar
	Negative      bool     `json:"negative,omitempty"`       // topik tidak ada di dokumen: pencarian tidak boleh mengembalikan dokumen (AI menjawab tanpa mengutip dokumen)
	FilterModule  string   `json:"filter_module,omitempty"`  // ALL | APPLICATION | EVIDENCE
}

// EvalCaseResult adalah hasil satu pertanyaan uji.
type EvalCaseResult struct {
	ID           string   `json:"id"`
	Question     string   `json:"question"`
	Negative     bool     `json:"negative"`
	Returned     []string `json:"returned"`      // standardized_name dokumen terpilih, urut peringkat
	DocRank      int      `json:"doc_rank"`      // peringkat dokumen benar pertama (1-based); 0 = tidak ditemukan
	ChunkJudged  bool     `json:"chunk_judged"`  // expect_phrases diisi sehingga Hit@3 dinilai
	ChunkHit     bool     `json:"chunk_hit"`     // frasa ditemukan di ≤3 potongan konteks dokumen yang benar
	Guarded      bool     `json:"guarded"`       // tidak ada dokumen yang dikembalikan (Document Guard)
	FollowUp     bool     `json:"follow_up"`     // dipahami sebagai pertanyaan lanjutan
	ContextChars int      `json:"context_chars"` // panjang konteks isi dokumen yang akan dikirim ke LLM
	LatencyMs    float64  `json:"latency_ms"`
	Error        string   `json:"error,omitempty"`
}

// EvalSummary adalah metrik agregat (definisi di docs/RAG_IMPROVEMENT_PLAN.md §6).
type EvalSummary struct {
	Cases           int     `json:"cases"`
	Positive        int     `json:"positive"`
	Negative        int     `json:"negative"`
	RecallAt4       float64 `json:"recall_at_4"`
	HitAt3          float64 `json:"hit_at_3"`
	MRR             float64 `json:"mrr"`
	GuardAccuracy   float64 `json:"guard_accuracy"`
	P95LatencyMs    float64 `json:"p95_latency_ms"`
	AvgContextChars float64 `json:"avg_context_chars"`
}

// EvalReport adalah keluaran satu kali evaluasi, disimpan sebagai JSON untuk dibandingkan dengan baseline.
type EvalReport struct {
	RunAt        time.Time         `json:"run_at"`
	IndexVersion int               `json:"index_version"`
	Summary      EvalSummary       `json:"summary"`
	Results      []EvalCaseResult  `json:"results"`
	Rerank       *RerankEvalReport `json:"rerank,omitempty"` // perbandingan heuristik vs reranker model (flag -rerank)
}

// RunRetrievalEval menjalankan setiap pertanyaan uji lewat RetrieveContext dengan engine builtin (deterministik,
// tanpa biaya kuota LLM) sebagai user yang diberikan, sehingga isolasi tim ikut teruji.
func RunRetrievalEval(db *gorm.DB, user models.User, cases []EvalCase) EvalReport {
	cfg := LLMConfig{Provider: ProviderBuiltin}
	results := make([]EvalCaseResult, 0, len(cases))
	for _, c := range cases {
		var history []ChatTurn
		var err error
		for _, q := range c.History {
			var prev *RetrievalResult
			prev, err = RetrieveContext(db, user, AIChatRequest{Prompt: q, FilterModule: c.FilterModule}, history, cfg)
			if err != nil {
				break
			}
			turn := ChatTurn{Prompt: q}
			for _, d := range prev.Documents {
				turn.DocumentIDs = append(turn.DocumentIDs, d.ID)
				turn.DocTitles = append(turn.DocTitles, d.Title+" ("+d.VersionLabel+")")
			}
			history = append(history, turn)
		}

		var r *RetrievalResult
		start := time.Now()
		if err == nil {
			r, err = RetrieveContext(db, user, AIChatRequest{Prompt: c.Question, FilterModule: c.FilterModule}, history, cfg)
		}
		res := judgeEvalCase(c, r)
		res.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
		if err != nil {
			res.Error = err.Error()
		}
		results = append(results, res)
	}
	return EvalReport{RunAt: time.Now(), IndexVersion: contentIndexVersion, Summary: SummarizeEval(results), Results: results}
}

// judgeEvalCase menilai hasil retrieval satu pertanyaan terhadap jawaban yang diharapkan.
func judgeEvalCase(c EvalCase, r *RetrievalResult) EvalCaseResult {
	res := EvalCaseResult{ID: c.ID, Question: c.Question, Negative: c.Negative, ChunkJudged: !c.Negative && len(c.ExpectPhrases) > 0}
	if r == nil {
		res.Guarded = true
		return res
	}
	res.FollowUp = r.FollowUp
	res.Guarded = len(r.Documents) == 0

	expected := map[string]bool{}
	for _, name := range c.ExpectDocs {
		expected[strings.ToLower(name)] = true
	}
	for i, d := range r.Documents {
		res.Returned = append(res.Returned, d.StandardizedName)
		for _, chunk := range r.ContextChunks[d.ID] {
			res.ContextChars += len(chunk.Text)
		}
		if !expected[strings.ToLower(d.StandardizedName)] {
			continue
		}
		if res.DocRank == 0 {
			res.DocRank = i + 1
		}
		if res.ChunkJudged && !res.ChunkHit {
			res.ChunkHit = chunksContainPhrases(r.ContextChunks[d.ID], c.ExpectPhrases)
		}
	}
	return res
}

// chunksContainPhrases: setiap frasa muncul (tanpa membedakan huruf besar/kecil) di salah satu dari ≤3 potongan pertama.
func chunksContainPhrases(chunks []chunkHit, phrases []string) bool {
	if len(chunks) > 3 {
		chunks = chunks[:3]
	}
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString(strings.Join(strings.Fields(strings.ToLower(c.Text)), " "))
		sb.WriteString("\n")
	}
	text := sb.String()
	for _, p := range phrases {
		if !strings.Contains(text, strings.Join(strings.Fields(strings.ToLower(p)), " ")) {
			return false
		}
	}
	return true
}

// SummarizeEval menghitung metrik agregat dari hasil per pertanyaan.
func SummarizeEval(results []EvalCaseResult) EvalSummary {
	s := EvalSummary{Cases: len(results)}
	var recall, rr, chunkHits, chunkJudged, guard, contextChars float64
	latencies := make([]float64, 0, len(results))
	for _, r := range results {
		latencies = append(latencies, r.LatencyMs)
		if r.Negative {
			s.Negative++
			if r.Guarded {
				guard++
			}
			continue
		}
		s.Positive++
		contextChars += float64(r.ContextChars)
		if r.DocRank > 0 && r.DocRank <= 4 {
			recall++
			rr += 1 / float64(r.DocRank)
		}
		if r.ChunkJudged {
			chunkJudged++
			if r.ChunkHit {
				chunkHits++
			}
		}
	}
	if s.Positive > 0 {
		s.RecallAt4 = round3(recall / float64(s.Positive))
		s.MRR = round3(rr / float64(s.Positive))
		s.AvgContextChars = math.Round(contextChars / float64(s.Positive))
	}
	if chunkJudged > 0 {
		s.HitAt3 = round3(chunkHits / chunkJudged)
	}
	if s.Negative > 0 {
		s.GuardAccuracy = round3(guard / float64(s.Negative))
	}
	if len(latencies) > 0 {
		sort.Float64s(latencies)
		idx := int(math.Ceil(0.95*float64(len(latencies)))) - 1
		s.P95LatencyMs = math.Round(latencies[idx]*10) / 10
	}
	return s
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
