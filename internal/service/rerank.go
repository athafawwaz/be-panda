package service

import (
	"math"
	"sort"
	"strings"

	"panda-backend/internal/models"
)

const (
	rrfK             = 60 // konstanta Reciprocal Rank Fusion
	maxSelectedDocs  = 6  // dokumen maksimal yang dipakai sebagai bukti jawaban mode Thinking
	maxChunksPerDoc  = 3  // potongan maksimal per dokumen yang dipilih sebagai bukti
	proximityScaleTk = 10 // jarak kata (token) yang membuat skor kedekatan turun setengah
)

// evidence adalah satu potongan isi dokumen yang ditemukan agen; satu potongan bisa ditemukan beberapa query.
type evidence struct {
	Ref        string // E1, E2, … dipakai agen LLM & tampilan langkah
	Doc        *models.Document
	ChunkID    string
	ChunkIndex int
	Text       string
	FromMeta   bool    // ditemukan dari kecocokan judul/ringkasan dokumen, bukan isi
	BestBM25   float64 // skor BM25 tertinggi di antara query yang menemukannya
	RRF        float64 // akumulasi 1/(rrfK + peringkat) dari setiap query
	Hits       int     // jumlah query yang menemukannya
	AgentRank  int     // urutan pilihan agen saat menyimpulkan (1 = paling relevan); 0 = tidak dipilih
	Neighbors  []chunkHit
	Features   rerankFeatures
	Score      float64 // skor akhir 0–1 setelah rerank
}

// rerankFeatures adalah sinyal relevansi (masing-masing 0–1) yang digabung menjadi skor akhir.
type rerankFeatures struct {
	RRF       float64 // seberapa sering & tinggi potongan ini muncul di berbagai query
	BM25      float64 // kecocokan kata terbaik (dinormalisasi terhadap temuan terbaik)
	Coverage  float64 // porsi kata kunci pertanyaan yang ada di potongan
	Proximity float64 // kata kunci yang berbeda muncul berdekatan
	Metadata  float64 // kata kunci cocok dengan judul/kategori/aplikasi dokumen
	Freshness float64 // dokumen dari versi (section) terbaru
	Agent     float64 // dipilih agen LLM sebagai bukti, sesuai urutannya
}

// addRanking menambahkan hasil satu query (sudah urut relevansi) ke skor RRF temuan.
func addRanking(ranked []*evidence) {
	for i, ev := range ranked {
		ev.RRF += 1 / float64(rrfK+i+1)
		ev.Hits++
	}
}

// rerankEvidence menilai ulang semua temuan: gabungan RRF lintas query, BM25, cakupan & kedekatan kata kunci,
// kecocokan metadata, versi terbaru, dan pilihan agen (bila ada). Hasilnya diurutkan menurun berdasarkan Score.
func rerankEvidence(pool []*evidence, focus []searchTerm, latest map[string]bool, metaSets map[string]map[string]bool) {
	maxRRF, maxBM25, picked := 0.0, 0.0, 0
	for _, ev := range pool {
		maxRRF = math.Max(maxRRF, ev.RRF)
		maxBM25 = math.Max(maxBM25, ev.BestBM25)
		if ev.AgentRank > 0 {
			picked++
		}
	}
	totalWeight := 0.0
	for _, t := range focus {
		totalWeight += t.Weight
	}

	for _, ev := range pool {
		f := rerankFeatures{Freshness: 1}
		if maxRRF > 0 {
			f.RRF = ev.RRF / maxRRF
		}
		if maxBM25 > 0 {
			f.BM25 = ev.BestBM25 / maxBM25
		}
		positions := termPositions(ev.Text, focus)
		if totalWeight > 0 {
			covered, meta := 0.0, 0.0
			for i, t := range focus {
				if len(positions[i]) > 0 {
					covered += t.Weight
				}
				if metaSets != nil && setHasTerm(metaSets[ev.Doc.ID], t.Term) {
					meta += t.Weight
				}
			}
			f.Coverage = covered / totalWeight
			f.Metadata = meta / totalWeight
		}
		f.Proximity = proximityScore(positions)
		if latest != nil && !latest[ev.Doc.ID] {
			f.Freshness = 0.5
		}
		if ev.AgentRank > 0 && picked > 0 {
			f.Agent = 1 - 0.5*float64(ev.AgentRank-1)/float64(picked)
		}
		ev.Features = f

		score := 0.30*f.RRF + 0.20*f.BM25 + 0.20*f.Coverage + 0.10*f.Proximity + 0.12*f.Metadata + 0.08*f.Freshness
		if picked > 0 {
			// Agen LLM sudah membaca bukti & menimbang konteks pertanyaan: pilihannya mendapat porsi 30%
			score = 0.70*score + 0.30*f.Agent
		}
		ev.Score = math.Round(score*1000) / 1000
	}
	sort.SliceStable(pool, func(i, j int) bool { return pool[i].Score > pool[j].Score })
}

// termPositions mengembalikan posisi kata (indeks token) setiap kata kunci di teks, dicocokkan dalam bentuk dasar.
func termPositions(text string, focus []searchTerm) [][]int {
	positions := make([][]int, len(focus))
	for pos, w := range splitWords(strings.ToLower(text)) {
		stem := stemWord(w)
		for i, t := range focus {
			if termMatches(stem, t.Term) || termMatches(w, t.Term) {
				positions[i] = append(positions[i], pos)
			}
		}
	}
	return positions
}

// proximityScore: jendela token terkecil yang memuat setidaknya dua kata kunci berbeda; makin rapat makin tinggi.
func proximityScore(positions [][]int) float64 {
	type occ struct{ pos, term int }
	var all []occ
	present := 0
	for i, ps := range positions {
		if len(ps) > 0 {
			present++
		}
		for _, p := range ps {
			all = append(all, occ{p, i})
		}
	}
	if present < 2 {
		return 0
	}
	sort.Slice(all, func(i, j int) bool { return all[i].pos < all[j].pos })
	best := math.MaxInt32
	for i := 1; i < len(all); i++ {
		if all[i].term != all[i-1].term {
			best = min(best, all[i].pos-all[i-1].pos)
		}
	}
	return 1 / (1 + float64(best-1)/proximityScaleTk)
}

// latestSectionDocs menandai dokumen yang berasal dari section (versi) terbaru untuk pasangan aplikasi & kategori
// yang sama. Dokumen tanpa aplikasi/section (Evidence) selalu dianggap terbaru.
func latestSectionDocs(docs []models.Document) map[string]bool {
	best := map[string]int{}
	key := func(d models.Document) string { return *d.ApplicationID + "|" + d.DocCategory }
	for _, d := range docs {
		if d.ApplicationID != nil && d.Section != nil && d.Section.SortOrder > best[key(d)] {
			best[key(d)] = d.Section.SortOrder
		}
	}
	latest := make(map[string]bool, len(docs))
	for _, d := range docs {
		latest[d.ID] = d.ApplicationID == nil || d.Section == nil || d.Section.SortOrder >= best[key(d)]
	}
	return latest
}

// selectEvidence memilih bukti terbaik dengan menjaga keragaman: maksimal maxChunksPerDoc potongan per dokumen
// dan maxSelectedDocs dokumen. Urutan dokumen mengikuti skor bukti terbaiknya.
func selectEvidence(ranked []*evidence) []*evidence {
	perDoc := map[string]int{}
	var docs []string
	var out []*evidence
	for _, ev := range ranked {
		if ev.Score <= 0 {
			continue
		}
		id := ev.Doc.ID
		if perDoc[id] == 0 {
			if len(docs) >= maxSelectedDocs {
				continue
			}
			docs = append(docs, id)
		}
		if perDoc[id] >= maxChunksPerDoc {
			continue
		}
		perDoc[id]++
		out = append(out, ev)
	}
	return out
}
