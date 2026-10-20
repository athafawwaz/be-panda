package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"panda-backend/internal/models"
	"panda-backend/internal/storage"
)

const (
	chunkTargetChars    = 1200 // ukuran ideal satu potongan isi dokumen
	chunkOverlapChars   = 200  // tumpang tindih antar potongan agar kalimat di perbatasan tidak terputus konteksnya
	extractPreviewChars = 2000 // cuplikan awal isi yang disimpan di documents.extracted_text
	maxChunkCandidates  = 1500 // batas baris chunk jalur LIKE lama (hanya selama re-index ke indeks kata)
	chunksPerDocument   = 3    // jumlah potongan isi per dokumen yang dikirim ke LLM
	leadingChunks       = 2    // dipakai bila dokumen cocok dari judul/ringkasan tapi tidak ada potongan isi yang cocok
	termInsertBatch     = 500  // 500 baris x 4 kolom tetap di bawah batas 2100 parameter SQL Server
	exactMatchBonus     = 1.2  // pengali skor chunk yang memuat kata persis seperti ditulis user (meredam over-stemming)

	// contentIndexVersion WAJIB dinaikkan setiap kali ekstraksi, normalisasi, chunking, tokenizer/stemming,
	// atau skema chunk berubah: dokumen di bawah versi ini diindeks ulang otomatis saat backend start.
	// v1: indeks kata (document_chunk_terms) + stemming Sastrawi + halaman tak terbaca.
	contentIndexVersion = 1
)

// ContentIndexVersion adalah versi pipeline indeks isi yang berlaku (dipakai alat seperti cmd/rag-eval).
const ContentIndexVersion = contentIndexVersion

// ContentIndexResult memberi tahu apakah isi file berhasil dibaca untuk Agentic AI.
type ContentIndexResult struct {
	Source      string `json:"source"` // FILE | SUMMARY
	Chunks      int    `json:"chunks"`
	Note        string `json:"note,omitempty"`
	TotalPages  int    `json:"total_pages,omitempty"`  // jumlah halaman PDF / slide / sheet
	UnreadPages int    `json:"unread_pages,omitempty"` // halaman bergambar tanpa teks (kemungkinan hasil scan)
	Truncated   bool   `json:"truncated,omitempty"`    // dokumen terlalu panjang, hanya bagian awal yang dibaca
}

// IndexDocumentContent membaca isi file, memecahnya menjadi potongan (chunk), dan menyimpannya beserta indeks
// kata per chunk sebagai bahan pencarian Agentic AI (RAG). Bila isi file tidak terbaca, ringkasan dipakai sebagai gantinya.
func IndexDocumentContent(db *gorm.DB, doc *models.Document, fileBytes []byte) ContentIndexResult {
	res := ContentIndexResult{Source: "FILE"}
	extracted, err := ExtractDocument(fileBytes, doc.FileExtension)
	content := extracted.FullText()
	res.TotalPages = extracted.TotalPages
	res.UnreadPages = len(extracted.EmptyPages)
	res.Truncated = extracted.Truncated

	var notes []string
	if content == "" {
		res.Source = "SUMMARY"
		switch {
		case len(fileBytes) == 0:
			notes = append(notes, "Tidak ada file yang diunggah, AI membaca ringkasan dokumen.")
		case errors.Is(err, ErrUnsupportedFormat):
			notes = append(notes, fmt.Sprintf("Isi file %s belum bisa dibaca, AI hanya membaca ringkasan. Gunakan PDF, DOCX, XLSX, PPTX, atau TXT.", doc.FileExtension))
		case err != nil:
			log.Printf("[RAG] gagal membaca isi dokumen %s: %v", doc.ID, err)
			notes = append(notes, "Isi file tidak dapat dibaca, AI hanya membaca ringkasan.")
		default:
			notes = append(notes, "File tidak berisi teks (kemungkinan hasil scan), AI hanya membaca ringkasan.")
		}
		content = doc.AISummary
	} else if res.UnreadPages > 0 {
		notes = append(notes, unreadPagesNote(extracted))
	}
	if extracted.Truncated && res.Source == "FILE" {
		notes = append(notes, fmt.Sprintf("Dokumen sangat panjang: hanya ±%d ribu karakter pertama yang dibaca AI.", maxExtractedChars/1000))
	}
	res.Note = strings.Join(notes, " ")

	chunks := ChunkText(content)
	preview := cutUTF8(content, extractPreviewChars)
	now := time.Now()

	chunkRows := make([]models.DocumentChunk, len(chunks))
	var termRows []models.DocumentChunkTerm
	for i, c := range chunks {
		id := uuid.NewString()
		chunkRows[i] = models.DocumentChunk{ID: id, DocumentID: doc.ID, ChunkIndex: i, ChunkText: c, TokenCount: len(splitWords(c))}
		for term, tf := range termFrequencies(c) {
			termRows = append(termRows, models.DocumentChunkTerm{ChunkID: id, Term: term, DocumentID: doc.ID, TF: tf})
		}
	}
	var pageCount *int
	if extracted.TotalPages > 0 {
		pageCount = &extracted.TotalPages
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("document_id = ?", doc.ID).Delete(&models.DocumentChunkTerm{}).Error; err != nil {
			return err
		}
		if err := tx.Where("document_id = ?", doc.ID).Delete(&models.DocumentChunk{}).Error; err != nil {
			return err
		}
		if len(chunkRows) > 0 {
			// 100 baris x 6 kolom tetap di bawah batas 2100 parameter SQL Server
			if err := tx.CreateInBatches(chunkRows, 100).Error; err != nil {
				return err
			}
		}
		if len(termRows) > 0 {
			if err := tx.CreateInBatches(termRows, termInsertBatch).Error; err != nil {
				return err
			}
		}
		return tx.Model(&models.Document{}).Where("id = ?", doc.ID).Updates(map[string]interface{}{
			"extracted_text":            preview,
			"content_indexed_at":        now,
			"content_chunk_count":       len(chunks),
			"content_index_version":     contentIndexVersion,
			"content_page_count":        pageCount,
			"content_unread_page_count": res.UnreadPages,
		}).Error
	})
	if err != nil {
		log.Printf("[RAG] gagal menyimpan indeks isi dokumen %s: %v", doc.ID, err)
		return ContentIndexResult{Source: res.Source, Note: "Dokumen tersimpan, tetapi isi file gagal diindeks untuk AI."}
	}

	doc.ExtractedText = preview
	doc.ContentIndexedAt = &now
	doc.ContentChunkCount = len(chunks)
	doc.ContentIndexVersion = contentIndexVersion
	doc.ContentPageCount = pageCount
	doc.ContentUnreadPageCount = res.UnreadPages
	res.Chunks = len(chunks)
	return res
}

// unreadPagesNote: "2 dari 12 halaman tidak terbaca (hal. 11, 12; ...)", maksimal 6 nomor halaman disebut.
func unreadPagesNote(r ExtractResult) string {
	nums := make([]string, 0, len(r.EmptyPages))
	for i, p := range r.EmptyPages {
		if i == 6 {
			nums = append(nums, "…")
			break
		}
		nums = append(nums, fmt.Sprint(p))
	}
	return fmt.Sprintf("%d dari %d halaman tidak terbaca (hal. %s; kemungkinan hasil scan), sehingga isinya belum bisa dicari AI.",
		len(r.EmptyPages), r.TotalPages, strings.Join(nums, ", "))
}

// ReindexPendingDocuments mengindeks isi dokumen yang belum pernah dibaca AI atau diindeks dengan versi pipeline
// lama (contentIndexVersion). Dijalankan di background saat startup.
func ReindexPendingDocuments(db *gorm.DB) {
	var docs []models.Document
	if err := db.Select("id", "file_path", "file_extension", "ai_summary", "content_chunk_count").
		Where("content_indexed_at IS NULL OR content_index_version < ?", contentIndexVersion).
		Find(&docs).Error; err != nil {
		log.Printf("[RAG] gagal memeriksa dokumen yang belum diindeks: %v", err)
		return
	}
	if len(docs) == 0 {
		return
	}

	log.Printf("[RAG] Mengindeks isi %d dokumen (indeks v%d)...", len(docs), contentIndexVersion)
	fromFile, fromChunks := 0, 0
	for i := range docs {
		data, readErr := storage.Read(context.Background(), docs[i].FilePath)
		if readErr != nil && docs[i].ContentChunkCount > 0 {
			// File asli tidak terbaca dari storage (lokal/MinIO): pertahankan potongan isi yang sudah tersimpan dan cukup bangun ulang
			// indeks katanya, daripada menggantinya dengan ringkasan.
			if err := rebuildTermIndex(db, &docs[i]); err == nil {
				fromChunks++
				continue
			} else {
				log.Printf("[RAG] gagal membangun indeks kata dokumen %s: %v", docs[i].ID, err)
			}
		}
		if IndexDocumentContent(db, &docs[i], data).Source == "FILE" {
			fromFile++
		}
		if (i+1)%50 == 0 {
			log.Printf("[RAG] ... %d/%d dokumen diindeks", i+1, len(docs))
		}
	}
	log.Printf("[RAG] Selesai: %d dokumen diindeks (%d dari isi file, %d dari potongan tersimpan karena file tidak ditemukan, sisanya dari ringkasan).",
		len(docs), fromFile, fromChunks)
}

// rebuildTermIndex membangun ulang indeks kata & panjang chunk dari potongan isi yang sudah tersimpan,
// tanpa membaca file asli. Dipakai saat file dokumen tidak lagi ada di disk.
func rebuildTermIndex(db *gorm.DB, doc *models.Document) error {
	var chunks []models.DocumentChunk
	if err := db.Select("id", "chunk_text").Where("document_id = ?", doc.ID).Find(&chunks).Error; err != nil {
		return err
	}
	if len(chunks) == 0 {
		return errors.New("tidak ada potongan isi tersimpan")
	}
	var termRows []models.DocumentChunkTerm
	for _, c := range chunks {
		for term, tf := range termFrequencies(c.ChunkText) {
			termRows = append(termRows, models.DocumentChunkTerm{ChunkID: c.ID, Term: term, DocumentID: doc.ID, TF: tf})
		}
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("document_id = ?", doc.ID).Delete(&models.DocumentChunkTerm{}).Error; err != nil {
			return err
		}
		for _, c := range chunks {
			if err := tx.Model(&models.DocumentChunk{}).Where("id = ?", c.ID).
				Update("token_count", len(splitWords(c.ChunkText))).Error; err != nil {
				return err
			}
		}
		if len(termRows) > 0 {
			if err := tx.CreateInBatches(termRows, termInsertBatch).Error; err != nil {
				return err
			}
		}
		return tx.Model(&models.Document{}).Where("id = ?", doc.ID).Update("content_index_version", contentIndexVersion).Error
	})
}

// ChunkText memecah isi dokumen per baris menjadi potongan ±chunkTargetChars karakter,
// dengan beberapa baris terakhir diulang di awal potongan berikutnya.
func ChunkText(text string) []string {
	var segments []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		for len(line) > chunkTargetChars {
			cut := strings.LastIndexByte(line[:chunkTargetChars], ' ')
			if cut <= 0 {
				cut = chunkTargetChars
				for cut > 0 && !isRuneStart(line[cut]) {
					cut--
				}
			}
			segments = append(segments, strings.TrimSpace(line[:cut]))
			line = strings.TrimSpace(line[cut:])
		}
		if line != "" {
			segments = append(segments, line)
		}
	}

	var chunks, cur []string
	curLen := 0
	for _, seg := range segments {
		if curLen > 0 && curLen+len(seg)+1 > chunkTargetChars {
			chunks = append(chunks, strings.Join(cur, "\n"))
			var keep []string
			keepLen := 0
			for i := len(cur) - 1; i >= 0 && keepLen+len(cur[i])+1 <= chunkOverlapChars; i-- {
				keep = append([]string{cur[i]}, keep...)
				keepLen += len(cur[i]) + 1
			}
			cur, curLen = keep, keepLen
		}
		cur = append(cur, seg)
		curLen += len(seg) + 1
	}
	if len(cur) > 0 {
		chunks = append(chunks, strings.Join(cur, "\n"))
	}
	return chunks
}

// ─── Retrieval ───────────────────────────────────────────────────────────────

type chunkHit struct {
	ChunkID    string
	DocumentID string
	ChunkIndex int
	Text       string // diisi loadChunkTexts hanya untuk dokumen yang dipakai sebagai konteks
	Score      float64
}

func splitWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// termMatches: kata pendek harus sama persis ("uat" tidak cocok dengan "buat"),
// kata ≥4 huruf boleh cocok sebagai awalan ("aplikasi" cocok dengan "aplikasinya").
func termMatches(word, term string) bool {
	return word == term || (len(term) >= 4 && strings.HasPrefix(word, term))
}

func escapeLike(s string) string {
	return strings.NewReplacer("[", "[[]", "%", "[%]", "_", "[_]").Replace(s)
}

// textHasTerm: kata kunci muncul sebagai kata (bukan potongan kata) di dalam teks.
func textHasTerm(text, term string) bool {
	for _, w := range splitWords(strings.ToLower(text)) {
		if termMatches(w, term) {
			return true
		}
	}
	return false
}

// retrieveRelevantChunks mencari potongan isi dokumen yang paling cocok dengan pertanyaan (skor BM25),
// beserta kata kunci yang muncul di isi tiap dokumen. scopedIDs membangun subquery id dokumen
// yang boleh diakses user sehingga isolasi tim tetap berlaku.
func retrieveRelevantChunks(db *gorm.DB, scopedIDs func() *gorm.DB, terms []searchTerm) (map[string][]chunkHit, map[string]map[string]bool) {
	if len(terms) == 0 {
		return nil, nil
	}
	// Selama re-index ke versi terbaru berjalan, sebagian dokumen belum punya indeks kata:
	// pakai jalur LIKE lama untuk semua dokumen agar hasil pencarian tidak bolong dan skornya sebanding.
	var stale int64
	db.Model(&models.Document{}).Where("id IN (?)", scopedIDs()).
		Where("content_indexed_at IS NULL OR content_index_version < ?", contentIndexVersion).Count(&stale)
	if stale > 0 {
		return retrieveChunksByLike(db, scopedIDs, terms)
	}
	return retrieveChunksByIndex(db, scopedIDs, terms)
}

// chunkTermStats adalah frekuensi tiap kata kunci (indeks sama dengan daftar terms) dalam satu chunk.
type chunkTermStats struct {
	ChunkID    string
	DocumentID string
	Length     int // jumlah kata chunk
	TF         []int
}

type termRow struct {
	ChunkID    string
	DocumentID string
	Term       string
	TF         int
	TokenCount int
}

// retrieveChunksByIndex membaca indeks kata (document_chunk_terms): query memakai index (term, document_id),
// sehingga df, jumlah chunk, dan panjang rata-rata dihitung tepat atas seluruh dokumen dalam cakupan user.
func retrieveChunksByIndex(db *gorm.DB, scopedIDs func() *gorm.DB, terms []searchTerm) (map[string][]chunkHit, map[string]map[string]bool) {
	exact := make([]string, 0, len(terms))
	for _, t := range terms {
		exact = append(exact, t.Term)
	}
	conds := []string{"t.term IN ?"}
	args := []interface{}{exact}
	for _, t := range terms {
		if len(t.Term) >= 4 {
			// Awalan kata tetap memakai index (sargable): "deploy" cocok dengan "deployment"
			conds = append(conds, "t.term LIKE ?")
			args = append(args, escapeLike(t.Term)+"%")
		}
	}

	var rows []termRow
	if err := db.Table("document_chunk_terms AS t").
		Select("t.chunk_id, t.document_id, t.term, t.tf, c.token_count").
		Joins("JOIN document_chunks c ON c.id = t.chunk_id").
		Where("t.document_id IN (?)", scopedIDs()).
		Where("("+strings.Join(conds, " OR ")+")", args...).
		Scan(&rows).Error; err != nil {
		log.Printf("[RAG] gagal mencari indeks kata: %v", err)
		return nil, nil
	}
	if len(rows) == 0 {
		return nil, nil
	}

	byChunk := map[string]*chunkTermStats{}
	var stats []*chunkTermStats
	for _, r := range rows {
		s := byChunk[r.ChunkID]
		if s == nil {
			s = &chunkTermStats{ChunkID: r.ChunkID, DocumentID: r.DocumentID, Length: r.TokenCount, TF: make([]int, len(terms))}
			byChunk[r.ChunkID] = s
			stats = append(stats, s)
		}
		for i, t := range terms {
			if termMatches(r.Term, t.Term) {
				s.TF[i] += r.TF
			}
		}
	}

	var totalChunks int64
	db.Model(&models.DocumentChunk{}).Where("document_id IN (?)", scopedIDs()).Count(&totalChunks)
	var avgLen float64
	db.Model(&models.DocumentChunk{}).Select("COALESCE(AVG(CAST(token_count AS FLOAT)), 0)").
		Where("document_id IN (?)", scopedIDs()).Scan(&avgLen)

	flat := make([]chunkTermStats, len(stats))
	for i, s := range stats {
		flat[i] = *s
	}
	return rankChunks(flat, terms, float64(totalChunks), avgLen)
}

// rankChunks menilai chunk dengan BM25 (k1 = 1,2; b = 0,75) dikali bobot kata kunci, lalu mengutamakan chunk yang
// memuat lebih banyak kata kunci berbeda. Mengembalikan maksimal chunksPerDocument hit per dokumen dan kata kunci
// (bentuk dasar) yang ditemukan di isi tiap dokumen.
func rankChunks(stats []chunkTermStats, terms []searchTerm, totalChunks, avgLen float64) (map[string][]chunkHit, map[string]map[string]bool) {
	const k1, b = 1.2, 0.75
	df := make([]int, len(terms))
	for _, s := range stats {
		for i, f := range s.TF {
			if f > 0 {
				df[i]++
			}
		}
	}
	n := math.Max(totalChunks, float64(len(stats)))
	if avgLen <= 0 {
		avgLen = 1
	}

	byDoc := map[string][]chunkHit{}
	docTerms := map[string]map[string]bool{}
	for _, s := range stats {
		score, distinct := 0.0, 0
		for i, f := range s.TF {
			if f == 0 {
				continue
			}
			distinct++
			if docTerms[s.DocumentID] == nil {
				docTerms[s.DocumentID] = map[string]bool{}
			}
			docTerms[s.DocumentID][terms[i].Term] = true
			idf := math.Log(1 + (n-float64(df[i])+0.5)/(float64(df[i])+0.5))
			tf := float64(f)
			length := math.Max(1, float64(s.Length))
			score += terms[i].Weight * idf * tf * (k1 + 1) / (tf + k1*(1-b+b*length/avgLen))
		}
		if score == 0 {
			continue
		}
		// Potongan yang memuat lebih banyak kata kunci berbeda lebih diutamakan
		score *= 0.5 + 0.5*float64(distinct)/float64(len(terms))
		byDoc[s.DocumentID] = append(byDoc[s.DocumentID], chunkHit{ChunkID: s.ChunkID, DocumentID: s.DocumentID, Score: score})
	}

	for id, hits := range byDoc {
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
		if len(hits) > chunksPerDocument {
			hits = hits[:chunksPerDocument]
		}
		byDoc[id] = hits
	}
	return byDoc, docTerms
}

// retrieveChunksByLike adalah jalur pencarian lama (LIKE atas chunk_text) yang hanya dipakai selama dokumen dalam
// cakupan masih diindeks ulang ke indeks kata. Kata dicocokkan dalam bentuk seperti ditulis user (tanpa stemming).
func retrieveChunksByLike(db *gorm.DB, scopedIDs func() *gorm.DB, terms []searchTerm) (map[string][]chunkHit, map[string]map[string]bool) {
	conds := make([]string, len(terms))
	args := make([]interface{}, len(terms))
	for i, t := range terms {
		// Batas kata di sisi SQL agar kata pendek seperti "uat" tidak ikut menarik "buat"/"muat"
		conds[i] = "(N' ' + chunk_text + N' ') LIKE ?"
		pattern := "%[^a-z0-9]" + escapeLike(t.Display)
		if len(t.Display) < 4 {
			pattern += "[^a-z0-9]%"
		} else {
			pattern += "%"
		}
		args[i] = pattern
	}

	var rows []models.DocumentChunk
	if err := db.Select("id", "document_id", "chunk_index", "chunk_text").
		Where("document_id IN (?)", scopedIDs()).
		Where(strings.Join(conds, " OR "), args...).
		Limit(maxChunkCandidates).
		Find(&rows).Error; err != nil {
		log.Printf("[RAG] gagal mencari isi dokumen: %v", err)
		return nil, nil
	}
	if len(rows) == 0 {
		return nil, nil
	}

	var totalChunks int64
	db.Model(&models.DocumentChunk{}).Where("document_id IN (?)", scopedIDs()).Count(&totalChunks)

	stats := make([]chunkTermStats, len(rows))
	totalLen := 0
	texts := map[string]models.DocumentChunk{}
	for i, r := range rows {
		words := splitWords(strings.ToLower(r.ChunkText))
		tf := make([]int, len(terms))
		for _, w := range words {
			for j, t := range terms {
				if termMatches(w, t.Display) {
					tf[j]++
				}
			}
		}
		stats[i] = chunkTermStats{ChunkID: r.ID, DocumentID: r.DocumentID, Length: len(words), TF: tf}
		totalLen += len(words)
		texts[r.ID] = r
	}

	byDoc, docTerms := rankChunks(stats, terms, float64(totalChunks), float64(totalLen)/float64(len(rows)))
	for id, hits := range byDoc {
		for i := range hits {
			hits[i].ChunkIndex = texts[hits[i].ChunkID].ChunkIndex
			hits[i].Text = texts[hits[i].ChunkID].ChunkText
		}
		byDoc[id] = hits
	}
	return byDoc, docTerms
}

// loadChunkTexts mengisi teks & urutan chunk untuk hit yang belum punya teks (hasil indeks kata).
func loadChunkTexts(db *gorm.DB, hitsByDoc map[string][]chunkHit) {
	var ids []string
	for _, hits := range hitsByDoc {
		for _, h := range hits {
			if h.Text == "" && h.ChunkID != "" {
				ids = append(ids, h.ChunkID)
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	var rows []models.DocumentChunk
	db.Select("id", "chunk_index", "chunk_text").Where("id IN ?", ids).Find(&rows)
	byID := make(map[string]models.DocumentChunk, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	for docID, hits := range hitsByDoc {
		for i := range hits {
			if r, ok := byID[hits[i].ChunkID]; ok && hits[i].Text == "" {
				hits[i].ChunkIndex = r.ChunkIndex
				hits[i].Text = r.ChunkText
			}
		}
		hitsByDoc[docID] = hits
	}
}

// rerankExactMatches memberi bonus pada chunk yang memuat kata persis seperti ditulis user, sehingga kecocokan
// langsung diutamakan di atas kecocokan hasil stemming ("pengujian" persis > "diuji").
func rerankExactMatches(hitsByDoc map[string][]chunkHit, terms []searchTerm) {
	for docID, hits := range hitsByDoc {
		for i := range hits {
			words := map[string]bool{}
			for _, w := range splitWords(strings.ToLower(hits[i].Text)) {
				words[w] = true
			}
			for _, t := range terms {
				if t.primary() && t.Display != t.Term && words[t.Display] {
					hits[i].Score *= exactMatchBonus
				}
			}
		}
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
		hitsByDoc[docID] = hits
	}
}

// loadLeadingChunks mengambil potongan awal dokumen, dipakai sebagai konteks bila dokumen cocok
// dari judul/ringkasan tetapi kata kuncinya tidak muncul di isi.
func loadLeadingChunks(db *gorm.DB, docIDs []string) map[string][]chunkHit {
	if len(docIDs) == 0 {
		return nil
	}
	var rows []models.DocumentChunk
	db.Select("id", "document_id", "chunk_index", "chunk_text").
		Where("document_id IN ? AND chunk_index < ?", docIDs, leadingChunks).
		Order("document_id, chunk_index").
		Find(&rows)

	out := map[string][]chunkHit{}
	for _, r := range rows {
		out[r.DocumentID] = append(out[r.DocumentID], chunkHit{ChunkID: r.ID, DocumentID: r.DocumentID, ChunkIndex: r.ChunkIndex, Text: r.ChunkText})
	}
	return out
}

// excerptKeys: kata yang dicari saat membuat cuplikan, bentuk tulisan user dulu lalu bentuk dasarnya.
func excerptKeys(terms []searchTerm) []string {
	keys := make([]string, 0, len(terms)*2)
	for _, t := range terms {
		keys = append(keys, t.Display)
		if t.Term != t.Display {
			keys = append(keys, t.Term)
		}
	}
	return keys
}

// makeExcerpt mengambil ±maxLen karakter di sekitar kata kunci pertama yang ditemukan.
func makeExcerpt(text string, terms []string, maxLen int) string {
	flat := strings.Join(strings.Fields(text), " ")
	lower := strings.ToLower(flat)
	pos := -1
	for _, t := range terms {
		if i := strings.Index(lower, t); i >= 0 && (pos < 0 || i < pos) {
			pos = i
		}
	}
	if pos < 0 || len(lower) != len(flat) {
		pos = 0
	}

	start := pos - maxLen/3
	if start < 0 {
		start = 0
	} else if sp := strings.IndexByte(flat[start:], ' '); sp >= 0 && sp < 30 {
		start += sp + 1
	}
	end := start + maxLen
	if end >= len(flat) {
		end = len(flat)
	} else if sp := strings.LastIndexByte(flat[start:end], ' '); sp > 0 {
		end = start + sp
	}
	for start < len(flat) && !isRuneStart(flat[start]) {
		start++
	}
	for end < len(flat) && !isRuneStart(flat[end]) {
		end--
	}

	excerpt := flat[start:end]
	if start > 0 {
		excerpt = "…" + excerpt
	}
	if end < len(flat) {
		excerpt += "…"
	}
	return excerpt
}
