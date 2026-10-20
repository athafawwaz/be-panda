// Command rag-eval mengukur kualitas retrieval Agentic AI terhadap set pertanyaan uji
// (docs/RAG_IMPROVEMENT_PLAN.md R6, §6). Dijalankan dari folder backend:
//
//	go run ./cmd/rag-eval -cases eval/rag_eval.json -out eval/hasil.json [-baseline eval/baseline.json] [-badge 2419901] [-rerank]
//
// -rerank membandingkan penilaian ulang heuristik vs reranker model (Super Admin → Docs → Reranker) pada kandidat
// pipeline tool search_documents yang sama (MRR, Hit@3, nDCG@5). Mode ini memanggil API reranker sehingga memakai kuotanya.
//
// Retrieval memakai engine builtin (tanpa LLM) sehingga hasilnya deterministik dan tidak memakai kuota.
// Bila -baseline diberikan, program keluar dengan kode 1 saat Recall@4 atau akurasi Document Guard turun.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/joho/godotenv"

	"panda-backend/internal/database"
	"panda-backend/internal/models"
	"panda-backend/internal/service"
	"panda-backend/internal/storage"
)

func main() {
	casesPath := flag.String("cases", "eval/rag_eval.json", "file JSON berisi pertanyaan uji")
	outPath := flag.String("out", "", "simpan laporan JSON ke file ini (opsional)")
	baselinePath := flag.String("baseline", "", "laporan JSON sebelumnya untuk dibandingkan (opsional)")
	badge := flag.String("badge", "", "jalankan sebagai user dengan badge ini (default: Super Admin, semua tim)")
	reindex := flag.Bool("reindex", false, "indeks ulang dokumen yang belum memakai indeks versi terbaru sebelum evaluasi")
	rerank := flag.Bool("rerank", false, "bandingkan heuristik vs reranker model (memakai kuota API reranker)")
	flag.Parse()

	_ = godotenv.Load(".env")
	_ = godotenv.Load("../.env")

	cases, err := loadCases(*casesPath)
	if err != nil {
		log.Fatalf("Gagal membaca pertanyaan uji: %v\nSalin eval/rag_eval.example.json menjadi %s lalu isi sesuai dokumen di database (lihat eval/README.md).", err, *casesPath)
	}

	db, err := database.Connect()
	if err != nil {
		log.Fatalf("Koneksi database gagal: %v", err)
	}
	if !db.Migrator().HasTable(&models.DocumentChunkTerm{}) {
		log.Fatalf("Tabel document_chunk_terms belum ada. Jalankan backend sekali (go run ./cmd/api) agar migrasi diterapkan.")
	}
	if err := storage.Init(storage.ConfigFromEnv()); err != nil {
		fmt.Printf("⚠ Storage: %v\n", err)
	}
	if *reindex {
		service.ReindexPendingDocuments(db)
	}
	var stale int64
	db.Model(&models.Document{}).Where("content_indexed_at IS NULL OR content_index_version < ?", service.ContentIndexVersion).Count(&stale)
	if stale > 0 {
		fmt.Printf("⚠ %d dokumen belum memakai indeks versi terbaru; pencarian memakai jalur LIKE lama. Tambahkan -reindex.\n\n", stale)
	}

	user := models.User{Role: models.RoleSuperAdmin, FullName: "rag-eval"}
	if *badge != "" {
		if err := db.Preload("Team").Preload("Branch").Where("badge_number = ?", *badge).First(&user).Error; err != nil {
			log.Fatalf("User dengan badge %s tidak ditemukan: %v", *badge, err)
		}
	}

	report := service.RunRetrievalEval(db, user, cases)
	printReport(report)

	if *rerank {
		rcfg := service.LoadRerankConfig(db)
		if !rcfg.Enabled() {
			log.Fatalf("Reranker model belum aktif. Atur di Super Admin → Docs → Reranker (provider, model, API key).")
		}
		rr := service.RunRerankEval(context.Background(), db, user, cases, rcfg, service.LoadLLMConfig(db).ShareDocContent)
		report.Rerank = &rr
		printRerankReport(rr)
	}

	if *outPath != "" {
		data, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(*outPath, data, 0644); err != nil {
			log.Fatalf("Gagal menyimpan laporan: %v", err)
		}
		fmt.Printf("\nLaporan disimpan di %s\n", *outPath)
	}

	if *baselinePath != "" {
		var base service.EvalReport
		data, err := os.ReadFile(*baselinePath)
		if err == nil {
			err = json.Unmarshal(data, &base)
		}
		if err != nil {
			log.Fatalf("Gagal membaca baseline: %v", err)
		}
		if regressed := compareWithBaseline(base, report); regressed {
			os.Exit(1)
		}
	}
}

func printRerankReport(r service.RerankEvalReport) {
	content := "judul + ringkasan (saklar isi dokumen mati)"
	if r.ContentShared {
		content = "judul + isi potongan"
	}
	fmt.Printf("\nPerbandingan reranker — %s, teks dikirim: %s\n", r.Reranker, content)
	fmt.Printf("%-8s %-10s %-10s %-8s %s\n", "ID", "HEURISTIK", "MODEL", "MS", "PERTANYAAN")
	rank := func(n int) string {
		if n == 0 {
			return "-"
		}
		return fmt.Sprint(n)
	}
	for _, c := range r.Results {
		note := ""
		if c.Fallback != "" {
			note = "  ⚠ " + c.Fallback
		}
		fmt.Printf("%-8s %-10s %-10s %-8d %s%s\n", c.ID, rank(c.HeuristicRank), rank(c.ModelRank), c.ModelLatencyMs, c.Question, note)
	}
	fmt.Printf("\n%-10s %-8s %-8s %-8s\n", "", "MRR", "Hit@3", "nDCG@5")
	fmt.Printf("%-10s %-8.3f %-8.3f %-8.3f\n", "Heuristik", r.Heuristic.MRR, r.Heuristic.HitAt3, r.Heuristic.NDCGAt5)
	fmt.Printf("%-10s %-8.3f %-8.3f %-8.3f\n", "Model", r.Model.MRR, r.Model.HitAt3, r.Model.NDCGAt5)
	fmt.Printf("%d pertanyaan, rata-rata latensi reranker %.0f ms\n", r.Cases, r.AvgLatencyMs)
}

func loadCases(path string) ([]service.EvalCase, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cases []service.EvalCase
	if err := json.Unmarshal(data, &cases); err != nil {
		return nil, err
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("file %s tidak berisi pertanyaan", path)
	}
	return cases, nil
}

func printReport(r service.EvalReport) {
	fmt.Printf("%-8s %-5s %-6s %-6s %-8s %s\n", "ID", "RANK", "CHUNK", "GUARD", "MS", "PERTANYAAN")
	for _, c := range r.Results {
		rank := "-"
		if c.DocRank > 0 {
			rank = fmt.Sprint(c.DocRank)
		}
		chunk := "n/a"
		if c.ChunkJudged {
			chunk = map[bool]string{true: "ya", false: "tidak"}[c.ChunkHit]
		}
		guard := ""
		if c.Guarded {
			guard = "guard"
		}
		status := ""
		switch {
		case c.Error != "":
			status = "  ✗ error: " + c.Error
		case c.Negative && !c.Guarded:
			status = "  ✗ seharusnya tidak ditemukan; dapat: " + strings.Join(c.Returned, ", ")
		case !c.Negative && c.DocRank == 0:
			status = "  ✗ dokumen benar tidak ditemukan; dapat: " + strings.Join(c.Returned, ", ")
		}
		fmt.Printf("%-8s %-5s %-6s %-6s %-8.1f %s%s\n", c.ID, rank, chunk, guard, c.LatencyMs, c.Question, status)
	}
	s := r.Summary
	fmt.Printf("\nIndeks v%d · %d pertanyaan (%d positif, %d negatif)\n", r.IndexVersion, s.Cases, s.Positive, s.Negative)
	fmt.Printf("Recall@4 %.3f · Hit@3 %.3f · MRR %.3f · Guard %.3f · p95 %.1f ms · konteks rata-rata %.0f karakter\n",
		s.RecallAt4, s.HitAt3, s.MRR, s.GuardAccuracy, s.P95LatencyMs, s.AvgContextChars)
}

// compareWithBaseline mencetak selisih metrik & pertanyaan yang memburuk; true bila Recall@4 atau Guard turun.
func compareWithBaseline(base, cur service.EvalReport) bool {
	b, c := base.Summary, cur.Summary
	fmt.Printf("\nDibanding baseline (%s):\n", base.RunAt.Format("02 Jan 2006 15:04"))
	fmt.Printf("  Recall@4 %+.3f · Hit@3 %+.3f · MRR %+.3f · Guard %+.3f · p95 %+.1f ms\n",
		c.RecallAt4-b.RecallAt4, c.HitAt3-b.HitAt3, c.MRR-b.MRR, c.GuardAccuracy-b.GuardAccuracy, c.P95LatencyMs-b.P95LatencyMs)

	prev := map[string]service.EvalCaseResult{}
	for _, r := range base.Results {
		prev[r.ID] = r
	}
	for _, r := range cur.Results {
		p, ok := prev[r.ID]
		if !ok {
			continue
		}
		worse := (p.DocRank > 0 && (r.DocRank == 0 || r.DocRank > p.DocRank)) ||
			(p.ChunkHit && !r.ChunkHit) || (r.Negative && p.Guarded && !r.Guarded)
		if worse {
			fmt.Printf("  ✗ %s memburuk: rank %d → %d, chunk %t → %t, guard %t → %t\n",
				r.ID, p.DocRank, r.DocRank, p.ChunkHit, r.ChunkHit, p.Guarded, r.Guarded)
		}
	}

	regressed := c.RecallAt4 < b.RecallAt4 || c.GuardAccuracy < b.GuardAccuracy
	if regressed {
		fmt.Println("\nGERBANG GAGAL: Recall@4 atau akurasi Document Guard turun dari baseline.")
	}
	return regressed
}
