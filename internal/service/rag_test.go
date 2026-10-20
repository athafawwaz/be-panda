package service

import (
	"strings"
	"testing"
	"unicode/utf8"

	"panda-backend/internal/models"
)

func findTerm(terms []searchTerm, display string) (searchTerm, bool) {
	for _, t := range terms {
		if t.Display == display {
			return t, true
		}
	}
	return searchTerm{}, false
}

// ─── Kata kunci (R2) ─────────────────────────────────────────────────────────

func TestContentSearchTermsKeepsAcronyms(t *testing.T) {
	// Pertanyaan panjang: dulu hanya 8 kata terpanjang yang dipakai sehingga "sap" & "uat" terbuang
	prompt := "Tolong jelaskan hasil pengujian aplikasi SAP modul keuangan perusahaan periode tahun 2024 terkait integrasi pembayaran vendor UAT"
	terms := contentSearchTerms(prompt, knownSearchWords(nil))
	for _, want := range []string{"uat", "sap", "2024", "pengujian", "keuangan"} {
		if _, ok := findTerm(terms, want); !ok {
			t.Errorf("kata %q harus dipertahankan, dapat %v", want, termDisplays(terms))
		}
	}
	if len(terms) > maxSearchTerms {
		t.Errorf("maksimal %d kata kunci, dapat %d", maxSearchTerms, len(terms))
	}
	if terms[0].Display != "uat" {
		t.Errorf("kode kategori yang dikenal harus paling depan, dapat %v", termDisplays(terms))
	}
	for _, stop := range []string{"tolong", "hasil", "terkait"} {
		if _, ok := findTerm(terms, stop); ok {
			t.Errorf("kata umum %q tidak boleh jadi kata kunci", stop)
		}
	}
}

func TestContentSearchTermsPrioritizesKnownCodes(t *testing.T) {
	known := map[string]bool{"corepay": true}
	terms := contentSearchTerms("dokumentasi lengkap perubahan konfigurasi corepay", known)
	if len(terms) == 0 || terms[0].Display != "corepay" {
		t.Fatalf("kode aplikasi dalam cakupan harus paling depan, dapat %v", termDisplays(terms))
	}
}

func TestContentSearchTermsStemsAndShortAcronyms(t *testing.T) {
	terms := contentSearchTerms("hasil QA di pengujian", nil)
	if got, ok := findTerm(terms, "pengujian"); !ok || got.Term != "uji" || got.Weight != 1 {
		t.Fatalf("\"pengujian\" harus dicari sebagai bentuk dasar \"uji\", dapat %+v", terms)
	}
	if _, ok := findTerm(terms, "qa"); !ok {
		t.Fatalf("singkatan 2 huruf yang ditulis kapital harus dipakai, dapat %v", termDisplays(terms))
	}
	if _, ok := findTerm(terms, "di"); ok {
		t.Fatalf("kata depan \"di\" tidak boleh jadi kata kunci")
	}
}

func TestKnownSearchWordsSkipsGenericAppWords(t *testing.T) {
	docs := []models.Document{{Application: &models.Application{AppCode: "SIMSKBDN", AppName: "Sistem Informasi Monitoring SKBDN Terintegrasi"}}}
	known := knownSearchWords(docs)
	for _, w := range []string{"simskbdn", "skbdn", "uat", "bast"} {
		if !known[w] {
			t.Errorf("%q harus dikenal", w)
		}
	}
	for _, w := range []string{"sistem", "monitoring", "lainnya"} {
		if known[w] {
			t.Errorf("kata umum %q tidak boleh diprioritaskan", w)
		}
	}
}

// ─── Stemming & sinonim (R5) ─────────────────────────────────────────────────

func TestIndexTermsStemsIndonesianWords(t *testing.T) {
	tf := termFrequencies("Pengujian aplikasinya diuji oleh tim QA pada 2024; penulisan laporan memverifikasi hasil.")
	want := map[string]int{"uji": 2, "aplikasi": 1, "qa": 1, "2024": 1, "tulis": 1, "verifikasi": 1}
	for term, n := range want {
		if tf[term] != n {
			t.Errorf("frekuensi %q = %d, seharusnya %d (semua: %v)", term, tf[term], n, tf)
		}
	}
	for _, stop := range []string{"tim", "pada", "hasil"} {
		if tf[stop] > 0 {
			t.Errorf("stop word %q tidak boleh diindeks", stop)
		}
	}
}

func TestStemWordKeepsCodesAndNumbers(t *testing.T) {
	for _, w := range []string{"corepay", "simskbdn", "bast", "v1", "2024", "sap"} {
		if got := stemWord(w); got != w {
			t.Errorf("%q tidak boleh berubah, dapat %q", w, got)
		}
	}
}

func TestSynonymExpansions(t *testing.T) {
	cases := []struct {
		prompt string
		want   string
	}{
		{"kapan serah terima CorePay dilakukan", "bast"},
		{"hasil pengujian SIMSKBDN", "uat"},
		{"BAST CorePay", "serah"},
		{"laporan uji penetrasi portal", "pentest"},
	}
	for _, c := range cases {
		base := contentSearchTerms(c.prompt, nil)
		exp := synonymExpansions(c.prompt, base)
		got, ok := findTerm(exp, c.want)
		if !ok {
			t.Errorf("%q harus menambah sinonim %q, dapat %v", c.prompt, c.want, termDisplays(exp))
			continue
		}
		if got.Weight != synonymTermWeight || got.primary() {
			t.Errorf("sinonim harus berbobot %.1f dan bukan kata utama, dapat %+v", synonymTermWeight, got)
		}
	}
	// Kata yang sudah ditulis user tidak ditambahkan lagi
	base := contentSearchTerms("BAST serah terima", nil)
	for _, e := range synonymExpansions("BAST serah terima", base) {
		if e.Term == "bast" || e.Term == "serah" {
			t.Errorf("sinonim duplikat: %+v", e)
		}
	}
}

// ─── Skor BM25 (R8) ──────────────────────────────────────────────────────────

func TestRankChunksPrefersCoverageAndPrimaryTerms(t *testing.T) {
	terms := []searchTerm{
		{Term: "corepay", Display: "corepay", Weight: 1},
		{Term: "uji", Display: "pengujian", Weight: 1},
		{Term: "bast", Display: "bast", Weight: synonymTermWeight},
	}
	stats := []chunkTermStats{
		{ChunkID: "a1", DocumentID: "A", Length: 100, TF: []int{2, 0, 0}},
		{ChunkID: "a2", DocumentID: "A", Length: 100, TF: []int{1, 1, 0}},
		{ChunkID: "a3", DocumentID: "A", Length: 100, TF: []int{1, 0, 0}},
		{ChunkID: "a4", DocumentID: "A", Length: 100, TF: []int{0, 0, 0}},
		{ChunkID: "a5", DocumentID: "A", Length: 400, TF: []int{1, 0, 0}},
		{ChunkID: "b1", DocumentID: "B", Length: 100, TF: []int{0, 1, 0}},
		{ChunkID: "b2", DocumentID: "B", Length: 100, TF: []int{0, 0, 1}},
	}
	byDoc, docTerms := rankChunks(stats, terms, 50, 100)

	if len(byDoc["A"]) != chunksPerDocument {
		t.Fatalf("maksimal %d chunk per dokumen, dapat %d", chunksPerDocument, len(byDoc["A"]))
	}
	if byDoc["A"][0].ChunkID != "a2" {
		t.Errorf("chunk yang memuat lebih banyak kata kunci berbeda harus teratas, dapat %+v", byDoc["A"])
	}
	for _, h := range byDoc["A"] {
		if h.ChunkID == "a4" || h.ChunkID == "a5" {
			t.Errorf("chunk tanpa kata kunci / lebih panjang dengan tf sama tidak boleh masuk 3 besar: %+v", byDoc["A"])
		}
	}
	if b := byDoc["B"]; len(b) != 2 || b[0].ChunkID != "b1" {
		t.Errorf("kata utama harus berbobot di atas sinonim, dapat %+v", b)
	}
	if !docTerms["A"]["corepay"] || !docTerms["A"]["uji"] || docTerms["A"]["bast"] || !docTerms["B"]["bast"] {
		t.Errorf("kata yang ditemukan per dokumen salah: %v", docTerms)
	}
}

func TestRerankExactMatchesPrefersUserWording(t *testing.T) {
	terms := []searchTerm{{Term: "uji", Display: "pengujian", Weight: 1}}
	hits := map[string][]chunkHit{"A": {
		{ChunkID: "1", Text: "Aplikasi telah diuji oleh vendor.", Score: 1.0},
		{ChunkID: "2", Text: "Hasil pengujian aplikasi: lulus.", Score: 0.9},
	}}
	rerankExactMatches(hits, terms)
	if hits["A"][0].ChunkID != "2" {
		t.Fatalf("chunk dengan kata persis seperti ditulis user harus naik, dapat %+v", hits["A"])
	}
}

// ─── Chunking & cuplikan ─────────────────────────────────────────────────────

func TestChunkTextSizesAndOverlap(t *testing.T) {
	var lines []string
	for i := 0; i < 120; i++ {
		lines = append(lines, "Baris ke-"+strings.Repeat("x", 5)+" berisi uraian pengujian aplikasi nomor "+string(rune('A'+i%26)))
	}
	chunks := ChunkText(strings.Join(lines, "\n"))
	if len(chunks) < 2 {
		t.Fatalf("teks panjang harus terpecah, dapat %d chunk", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > chunkTargetChars+chunkOverlapChars {
			t.Errorf("chunk %d terlalu panjang: %d", i, len(c))
		}
		if i > 0 {
			// Overlap: beberapa baris terakhir chunk sebelumnya (≤ chunkOverlapChars) diulang di awal chunk ini
			prevLines := strings.Split(chunks[i-1], "\n")
			lastLine := prevLines[len(prevLines)-1]
			head := c[:min(len(c), chunkOverlapChars+len(lastLine))]
			if !strings.Contains(head, lastLine) {
				t.Errorf("chunk %d harus mengulang baris terakhir chunk sebelumnya di bagian awal (overlap)", i)
			}
		}
	}
}

func TestChunkTextSplitsLongLineOnRuneBoundary(t *testing.T) {
	chunks := ChunkText(strings.Repeat("é", 1500))
	if len(chunks) < 2 {
		t.Fatalf("baris tanpa spasi lebih dari %d byte harus dipecah", chunkTargetChars)
	}
	for _, c := range chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("pemecahan memotong karakter multi-byte")
		}
	}
}

func TestTermMatchesAndEscapeLike(t *testing.T) {
	if termMatches("buat", "uat") {
		t.Errorf("\"uat\" tidak boleh cocok dengan \"buat\"")
	}
	if !termMatches("aplikasinya", "aplikasi") || termMatches("aplikasinya", "apl") {
		t.Errorf("awalan hanya untuk kata ≥4 huruf")
	}
	if got := escapeLike("50%_[x]"); got != "50[%][_][[]x]" {
		t.Errorf("escapeLike salah: %q", got)
	}
}

func TestMakeExcerptFindsStemmedTerm(t *testing.T) {
	text := strings.Repeat("Pendahuluan dokumen ini menjelaskan latar belakang. ", 20) + "Modul pembayaran telah diuji dan dinyatakan lulus pada 12 Oktober. " + strings.Repeat("Penutup ", 30)
	ex := makeExcerpt(text, excerptKeys([]searchTerm{{Term: "uji", Display: "pengujian", Weight: 1}}), 120)
	if !strings.Contains(ex, "diuji") || !utf8.ValidString(ex) {
		t.Fatalf("cuplikan harus berada di sekitar kata yang cocok, dapat %q", ex)
	}
}

func TestUnreadPagesNote(t *testing.T) {
	note := unreadPagesNote(ExtractResult{TotalPages: 12, EmptyPages: []int{11, 12}})
	if !strings.HasPrefix(note, "2 dari 12 halaman tidak terbaca (hal. 11, 12;") {
		t.Fatalf("catatan halaman tak terbaca salah: %q", note)
	}
}
