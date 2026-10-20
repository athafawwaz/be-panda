package service

import (
	"sort"
	"strings"
	"sync"
	"unicode"

	sastrawi "github.com/RadhiFadlillah/go-sastrawi"

	"panda-backend/internal/models"
)

const (
	maxSearchTerms     = 10  // kata kunci utama per pertanyaan
	maxExpansionTerms  = 4   // kata tambahan dari sinonim per pertanyaan
	maxIndexTermLength = 64  // batas kolom document_chunk_terms.term
	synonymTermWeight  = 0.7 // bobot kata hasil sinonim, di bawah kata yang ditulis user
)

// searchTerm adalah satu kata kunci pencarian isi dokumen.
type searchTerm struct {
	Term    string  // bentuk dasar (hasil stemming) yang dicocokkan dengan indeks kata
	Display string  // kata seperti ditulis user, untuk pesan "tidak ditemukan" dan bonus kecocokan persis
	Weight  float64 // 1 = kata dari pertanyaan; < 1 = sinonim atau konteks percakapan sebelumnya
}

// primary: kata yang benar-benar ditulis user (bukan sinonim/konteks), satu-satunya yang dilaporkan bila tidak ditemukan.
func (t searchTerm) primary() bool { return t.Weight >= 1 }

// contentStopWords: kata umum tambahan yang tidak berguna untuk mencari isi dokumen, di luar stopWords
// (yang juga dipakai skor metadata). Kata rujukan percakapan ("tersebut", "tadi") dibaca terpisah oleh deteksi
// pertanyaan lanjutan dari prompt mentah.
var contentStopWords = map[string]bool{
	"terkait": true, "perihal": true, "hasil": true, "tersebut": true, "tadi": true,
	"sebelumnya": true, "barusan": true, "lainnya": true,
}

func isSearchStopWord(w string) bool { return stopWords[w] || contentStopWords[w] }

var (
	stemmerOnce sync.Once
	stemmer     sastrawi.Stemmer
	stemCache   sync.Map // kata → bentuk dasar
)

// stemWord mengembalikan bentuk dasar kata Bahasa Indonesia (algoritma Nazief-Adriani, Sastrawi):
// "pengujian" & "diuji" → "uji", "penulisan" → "tulis". Kata pendek atau berangka (kode, versi, tahun)
// dibiarkan agar tetap cocok persis; kata di luar kamus (nama aplikasi, istilah asing) tidak berubah.
func stemWord(w string) string {
	if len(w) <= 3 || strings.IndexFunc(w, unicode.IsDigit) >= 0 {
		return w
	}
	if v, ok := stemCache.Load(w); ok {
		return v.(string)
	}
	stemmerOnce.Do(func() { stemmer = sastrawi.NewStemmer(sastrawi.DefaultDictionary()) })
	s := stemmer.Stem(w)
	if s == "" {
		s = w
	}
	stemCache.Store(w, s)
	return s
}

// indexTerms memecah teks menjadi kata indeks: huruf kecil, tanpa stop word, dalam bentuk dasar.
// Dipakai saat mengindeks chunk maupun saat membaca pertanyaan agar keduanya diproses identik.
func indexTerms(text string) []string {
	words := splitWords(strings.ToLower(text))
	out := make([]string, 0, len(words))
	for _, w := range words {
		if len(w) < 2 || len(w) > maxIndexTermLength || isSearchStopWord(w) {
			continue
		}
		if s := stemWord(w); !isSearchStopWord(s) {
			out = append(out, s)
		}
	}
	return out
}

// termFrequencies menghitung frekuensi tiap kata indeks dalam teks.
func termFrequencies(text string) map[string]int {
	tf := map[string]int{}
	for _, t := range indexTerms(text) {
		tf[t]++
	}
	return tf
}

// contentSearchTerms mengambil kata kunci pertanyaan untuk dicari di isi dokumen. Urutan prioritas:
// (1) kategori & kode/nama aplikasi yang dikenal, (2) singkatan yang ditulis kapital (SAP, QA) atau kata berangka
// (2024), (3) sisanya dari yang terpanjang. Dengan begitu singkatan pendek tidak terbuang lebih dulu.
func contentSearchTerms(prompt string, known map[string]bool) []searchTerm {
	type candidate struct {
		word string
		rank int
	}
	seen := map[string]bool{}
	var cands []candidate
	for _, raw := range splitWords(prompt) {
		w := strings.ToLower(raw)
		acronym := len(raw) >= 2 && raw == strings.ToUpper(raw) && strings.IndexFunc(raw, unicode.IsLetter) >= 0
		if seen[w] || isSearchStopWord(w) || len(w) > maxIndexTermLength || (len(w) < 3 && !acronym) {
			continue
		}
		seen[w] = true
		rank := 2
		switch {
		case known[w]:
			rank = 0
		case acronym || strings.IndexFunc(w, unicode.IsDigit) >= 0:
			rank = 1
		}
		cands = append(cands, candidate{w, rank})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].rank != cands[j].rank {
			return cands[i].rank < cands[j].rank
		}
		return cands[i].rank == 2 && len(cands[i].word) > len(cands[j].word)
	})

	var terms []searchTerm
	stems := map[string]bool{}
	for _, c := range cands {
		if len(terms) >= maxSearchTerms {
			break
		}
		s := stemWord(c.word)
		if stems[s] || isSearchStopWord(s) {
			continue
		}
		stems[s] = true
		terms = append(terms, searchTerm{Term: s, Display: c.word, Weight: 1})
	}
	return terms
}

// knownSearchWords: kode kategori serta kode/nama aplikasi dalam cakupan user, diprioritaskan sebagai kata kunci.
func knownSearchWords(docs []models.Document) map[string]bool {
	known := map[string]bool{}
	for _, c := range models.DocCategories {
		if c != "LAINNYA" {
			known[strings.ToLower(c)] = true
		}
	}
	for _, d := range docs {
		if d.Application == nil {
			continue
		}
		for _, w := range splitWords(strings.ToLower(d.Application.AppCode + " " + d.Application.AppName)) {
			if len(w) >= 3 && !genericAppWords[w] && !isSearchStopWord(w) {
				known[w] = true
			}
		}
	}
	return known
}

// ─── Sinonim ─────────────────────────────────────────────────────────────────

// categoryCodeExpansions: kode kategori → kata khas yang muncul di isi dokumen yang tidak menulis singkatannya
// (mis. BAST ditulis lengkap "Berita Acara Serah Terima").
var categoryCodeExpansions = map[string][]string{
	"prd":     {"requirement"},
	"uat":     {"uji"},
	"bast":    {"serah"},
	"fsd":     {"spesifikasi"},
	"sop":     {"prosedur"},
	"drc":     {"bencana"},
	"pentest": {"penetrasi"},
}

// extraSynonymTriggers melengkapi categoryKeywords (saran upload) dengan istilah yang lazim di pertanyaan.
var extraSynonymTriggers = map[string][]string{
	"UAT":  {"uji", "pengujian"},
	"BAST": {"serah terima"},
}

type synonymRule struct {
	trigger []string // urutan kata dasar yang harus muncul berurutan di pertanyaan
	expand  string
}

var (
	synonymOnce  sync.Once
	synonymRules []synonymRule
)

// loadSynonymRules menyusun aturan sinonim dari kata kunci kategori yang sama dengan saran upload (categoryKeywords):
// frasa seperti "serah terima" atau "uji penetrasi" menambahkan kode kategorinya ke pencarian.
func loadSynonymRules() []synonymRule {
	synonymOnce.Do(func() {
		add := func(code, phrase string) {
			trigger := indexTerms(phrase)
			if len(trigger) == 0 || (len(trigger) == 1 && trigger[0] == code) {
				return
			}
			synonymRules = append(synonymRules, synonymRule{trigger: trigger, expand: code})
		}
		for cat, phrases := range categoryKeywords {
			for _, p := range phrases {
				add(strings.ToLower(cat), p)
			}
		}
		for cat, phrases := range extraSynonymTriggers {
			for _, p := range phrases {
				add(strings.ToLower(cat), p)
			}
		}
		for code, words := range categoryCodeExpansions {
			for _, w := range words {
				synonymRules = append(synonymRules, synonymRule{trigger: []string{code}, expand: stemWord(w)})
			}
		}
		// Urutan tetap agar hasil ekspansi deterministik
		sort.Slice(synonymRules, func(i, j int) bool {
			a, b := strings.Join(synonymRules[i].trigger, " "), strings.Join(synonymRules[j].trigger, " ")
			if a != b {
				return a < b
			}
			return synonymRules[i].expand < synonymRules[j].expand
		})
	})
	return synonymRules
}

// synonymExpansions menambahkan kata sinonim (bobot lebih rendah) untuk pertanyaan: "serah terima" → bast,
// "pengujian" → uat, "BAST" → serah. Kata yang sudah ada di daftar pencarian tidak ditambahkan lagi.
func synonymExpansions(prompt string, existing []searchTerm) []searchTerm {
	stems := indexTerms(prompt)
	have := map[string]bool{}
	for _, t := range existing {
		have[t.Term] = true
	}
	var out []searchTerm
	for _, r := range loadSynonymRules() {
		if len(out) >= maxExpansionTerms {
			break
		}
		if have[r.expand] || !containsSequence(stems, r.trigger) {
			continue
		}
		have[r.expand] = true
		out = append(out, searchTerm{Term: r.expand, Display: r.expand, Weight: synonymTermWeight})
	}
	return out
}

func containsSequence(words, seq []string) bool {
	for i := 0; i+len(seq) <= len(words); i++ {
		match := true
		for j := range seq {
			if words[i+j] != seq[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
