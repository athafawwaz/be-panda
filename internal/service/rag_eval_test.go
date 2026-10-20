package service

import (
	"testing"

	"panda-backend/internal/models"
)

func TestJudgeEvalCase(t *testing.T) {
	r := &RetrievalResult{
		Documents: []models.Document{
			{ID: "a", StandardizedName: "COREPAY_UAT_v1.1"},
			{ID: "b", StandardizedName: "COREPAY_UAT_v1.0"},
		},
		ContextChunks: map[string][]chunkHit{
			"b": {{Text: "Ringkasan hasil:\nskenario login dinyatakan   LULUS."}},
		},
	}
	got := judgeEvalCase(EvalCase{ID: "q1", ExpectDocs: []string{"corepay_uat_V1.0"}, ExpectPhrases: []string{"dinyatakan lulus"}}, r)
	if got.DocRank != 2 || !got.ChunkJudged || !got.ChunkHit || got.Guarded {
		t.Fatalf("penilaian salah: %+v", got)
	}
	if len(got.Returned) != 2 || got.ContextChars == 0 {
		t.Fatalf("dokumen & panjang konteks harus dicatat: %+v", got)
	}

	miss := judgeEvalCase(EvalCase{ID: "q2", ExpectDocs: []string{"BAST_X"}, ExpectPhrases: []string{"lulus"}}, r)
	if miss.DocRank != 0 || miss.ChunkHit {
		t.Fatalf("dokumen benar tidak ada di hasil: %+v", miss)
	}

	guard := judgeEvalCase(EvalCase{ID: "n1", Negative: true}, &RetrievalResult{})
	if !guard.Guarded || guard.ChunkJudged {
		t.Fatalf("pertanyaan negatif tanpa hasil harus tercatat sebagai Document Guard: %+v", guard)
	}
}

func TestSummarizeEval(t *testing.T) {
	results := []EvalCaseResult{
		{DocRank: 1, ChunkJudged: true, ChunkHit: true, ContextChars: 1000, LatencyMs: 10},
		{DocRank: 2, ChunkJudged: true, ChunkHit: false, ContextChars: 500, LatencyMs: 20},
		{DocRank: 0, LatencyMs: 30},
		{DocRank: 5, LatencyMs: 40}, // di luar 4 besar: tidak dihitung recall
		{Negative: true, Guarded: true, LatencyMs: 50},
		{Negative: true, Guarded: false, LatencyMs: 200},
	}
	s := SummarizeEval(results)
	if s.Positive != 4 || s.Negative != 2 {
		t.Fatalf("jumlah kasus salah: %+v", s)
	}
	if s.RecallAt4 != 0.5 || s.MRR != 0.375 || s.HitAt3 != 0.5 || s.GuardAccuracy != 0.5 {
		t.Fatalf("metrik salah: %+v", s)
	}
	if s.P95LatencyMs != 200 || s.AvgContextChars != 375 {
		t.Fatalf("p95/konteks salah: %+v", s)
	}
}
