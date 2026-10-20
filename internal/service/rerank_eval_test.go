package service

import (
	"math"
	"testing"
)

func TestRerankEvalMetrics(t *testing.T) {
	expected := map[string]bool{"bast": true, "uat": true}
	if r := firstRelevantRank([]string{"sop", "uat", "bast"}, expected); r != 2 {
		t.Fatalf("peringkat dokumen benar pertama salah: %d", r)
	}
	// DCG = 1/log2(3) + 1/log2(4); IDCG = 1 + 1/log2(3)
	want := (1/math.Log2(3) + 0.5) / (1 + 1/math.Log2(3))
	if got := ndcgAt([]string{"sop", "uat", "bast"}, expected, 5); math.Abs(got-want) > 1e-9 {
		t.Fatalf("nDCG salah: %v ≠ %v", got, want)
	}
	if ndcgAt([]string{"uat", "bast"}, expected, 5) != 1 {
		t.Fatalf("urutan ideal harus bernilai 1")
	}
	m := summarizeRerank([]int{1, 4, 0}, []float64{1, 0.5, 0})
	if m.MRR != round3((1+0.25)/3) || m.HitAt3 != round3(1.0/3) || m.NDCGAt5 != 0.5 {
		t.Fatalf("ringkasan metrik salah: %+v", m)
	}
}
