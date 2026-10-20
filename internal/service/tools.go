package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"panda-backend/internal/models"
)

// Tool Agentic AI (format OpenAI "tools"). Tidak ada tool yang menerima parameter tim/branch/user: semua data berasal
// dari agentRun yang dibangun dengan cakupan akses user (scopedDocuments & ApplyFolderScope). Semua tool hanya membaca.

const (
	toolResultMaxChars     = 3500 // batas hasil tool (JSON) yang dikirim ke model
	toolOutputPreviewRunes = 1800 // cuplikan hasil yang disimpan di log langkah
	toolSnippetRunes       = 450  // isi potongan per temuan pencarian
	toolReadSectionRunes   = 1200 // isi per bagian pada read_document
	toolSummaryRunes       = 220
	listDocumentsDefault   = 10
	listDocumentsMax       = 20
	maxChunksPerDocResult  = 3
)

var (
	coreInitialCategories = []string{"PRD", "FSD", "UAT", "BAST"}
	coreChangeCategories  = []string{"PRD", "UAT", "BAST"}
)

// toolResult adalah hasil satu panggilan tool: JSON untuk model dan rincian untuk log langkah.
type toolResult struct {
	Output    string
	Summary   string
	Count     int
	Results   []StepResult
	Queries   []string
	Fresh     int // temuan/bacaan baru (untuk rem penelusuran jenuh)
	Err       string
	Truncated bool
	Rerank    *RerankInfo
}

type agentTool struct {
	Def   toolDef
	Title string
	Run   func(ctx context.Context, a *agentRun, args map[string]any, st *AgentStep) toolResult
}

func fnDef(name, desc string, props map[string]any, required ...string) toolDef {
	params := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		params["required"] = required
	}
	return toolDef{Type: "function", Function: toolFunctionDef{Name: name, Description: desc, Parameters: params}}
}

func strProp(desc string, enum ...string) map[string]any {
	p := map[string]any{"type": "string", "description": desc}
	if len(enum) > 0 {
		p["enum"] = enum
	}
	return p
}

func intProp(desc string, lo, hi int) map[string]any {
	return map[string]any{"type": "integer", "description": desc, "minimum": lo, "maximum": hi}
}

var agentToolset = []agentTool{
	{
		Title: "Mencari dokumen",
		Def: fnDef("search_documents",
			"Cari potongan isi dan metadata dokumen audit di repositori PANDA dalam cakupan akses user, lalu nilai ulang dengan reranker. "+
				"Pakai untuk pertanyaan apa pun tentang isi dokumen. Hasil memuat ref temuan (E#) dan ref dokumen (D#) untuk sitasi.",
			map[string]any{
				"query":    strProp("Kueri pencarian. Variasikan singkatan & kepanjangan, mis. UAT/pengujian, BAST/serah terima, SOP/prosedur."),
				"category": strProp("Batasi ke kategori dokumen.", models.DocCategories...),
				"app_code": strProp("Batasi ke aplikasi (kode aplikasi atau potongan namanya)."),
				"module":   strProp("Batasi ke modul.", "APPLICATION", "EVIDENCE"),
				"version":  strProp("Batasi ke label versi, mis. v1.1."),
				"top_k":    intProp("Jumlah temuan teratas yang dikembalikan (1–8).", 1, MaxRerankTopN),
			}, "query"),
		Run: runSearchDocumentsTool,
	},
	{
		Title: "Membaca bagian dokumen",
		Def: fnDef("read_document",
			"Baca bagian dokumen beserta bagian sebelum & sesudahnya agar konteksnya utuh. Pakai ref dari hasil tool lain (E# atau D#).",
			map[string]any{"ref": strProp("Ref temuan (mis. E2) atau dokumen (mis. D1). Boleh beberapa dipisah koma, maks 3.")}, "ref"),
		Run: runReadDocumentTool,
	},
	{
		Title: "Menyusun daftar dokumen",
		Def: fnDef("list_documents",
			"Daftar dokumen dalam cakupan user berdasarkan filter (untuk pertanyaan seperti 'dokumen terbaru', 'SOP apa saja', 'dokumen aplikasi X'). "+
				"Hanya metadata & ringkasan pengunggah, tanpa isi file.",
			map[string]any{
				"category":       strProp("Kategori dokumen.", models.DocCategories...),
				"app_code":       strProp("Kode atau potongan nama aplikasi."),
				"module":         strProp("Modul.", "APPLICATION", "EVIDENCE"),
				"version":        strProp("Label versi, mis. v1.0."),
				"title_contains": strProp("Kata yang harus ada di judul atau ringkasan."),
				"uploaded_after": strProp("Hanya yang diunggah sejak tanggal ini (YYYY-MM-DD)."),
				"sort":           strProp("Urutan.", "newest", "oldest", "title"),
				"limit":          intProp("Jumlah maksimal (1–20).", 1, listDocumentsMax),
			}),
		Run: runListDocumentsTool,
	},
	{
		Title: "Membuka profil aplikasi",
		Def: fnDef("get_application",
			"Profil satu aplikasi dalam cakupan user: folder, tim, setiap versi (section perubahan) beserta dokumen dan kategori dokumen inti yang belum ada.",
			map[string]any{"app": strProp("Kode aplikasi atau potongan namanya, mis. VISTA.")}, "app"),
		Run: runGetApplicationTool,
	},
	{
		Title: "Menghitung statistik repositori",
		Def: fnDef("get_repository_stats",
			"Jumlah dokumen dalam cakupan user, dikelompokkan per kategori, tim, modul, atau aplikasi.",
			map[string]any{"group_by": strProp("Pengelompokan.", "category", "team", "module", "application")}),
		Run: runRepositoryStatsTool,
	},
}

func toolDefinitions() []toolDef {
	defs := make([]toolDef, len(agentToolset))
	for i, t := range agentToolset {
		defs[i] = t.Def
	}
	return defs
}

func findTool(name string) *agentTool {
	for i := range agentToolset {
		if agentToolset[i].Def.Function.Name == name {
			return &agentToolset[i]
		}
	}
	return nil
}

func toolNames() []string {
	names := make([]string, len(agentToolset))
	for i, t := range agentToolset {
		names[i] = t.Def.Function.Name
	}
	return names
}

// ─── Argumen ─────────────────────────────────────────────────────────────────

func argString(args map[string]any, key string) string {
	switch v := args[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case []any:
		parts := make([]string, 0, len(v))
		for _, p := range v {
			if s, ok := p.(string); ok && strings.TrimSpace(s) != "" {
				parts = append(parts, strings.TrimSpace(s))
			}
		}
		return strings.Join(parts, ",")
	}
	return ""
}

func argInt(args map[string]any, key string, def, lo, hi int) int {
	n := def
	switch v := args[key].(type) {
	case float64:
		n = int(v)
	case string:
		if p, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			n = p
		}
	}
	return max(lo, min(n, hi))
}

func inList(v string, list []string) bool {
	for _, x := range list {
		if strings.EqualFold(v, x) {
			return true
		}
	}
	return false
}

// filterFromArgs membaca & memvalidasi filter dokumen. Pesan error ditulis agar model bisa memperbaiki panggilannya.
func filterFromArgs(args map[string]any) (docFilter, string) {
	f := docFilter{
		Category: strings.ToUpper(argString(args, "category")),
		AppCode:  argString(args, "app_code"),
		Module:   strings.ToUpper(argString(args, "module")),
		Version:  argString(args, "version"),
	}
	if f.Category != "" && !inList(f.Category, models.DocCategories) {
		return f, fmt.Sprintf("category %q tidak dikenal; pilihan: %s", f.Category, strings.Join(models.DocCategories, ", "))
	}
	if f.Module != "" && f.Module != "APPLICATION" && f.Module != "EVIDENCE" {
		return f, fmt.Sprintf("module %q tidak dikenal; pilihan: APPLICATION, EVIDENCE", f.Module)
	}
	return f, ""
}

func (f docFilter) asMap() map[string]string {
	m := map[string]string{}
	if f.Category != "" {
		m["category"] = f.Category
	}
	if f.AppCode != "" {
		m["app_code"] = f.AppCode
	}
	if f.Module != "" {
		m["module"] = f.Module
	}
	if f.Version != "" {
		m["version"] = f.Version
	}
	return m
}

// ─── Hasil JSON ──────────────────────────────────────────────────────────────

func errorResult(msg string) toolResult {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return toolResult{Output: string(b), Err: msg, Summary: msg}
}

// fitJSON menyusun JSON dengan n item, dikurangi sampai muat toolResultMaxChars. truncated = ada item yang dibuang.
func fitJSON(build func(n int) any, n int) (out string, shown int, truncated bool) {
	for k := n; k >= 0; k-- {
		b, _ := json.Marshal(build(k))
		if len(b) <= toolResultMaxChars || k == 0 {
			return string(b), k, k < n
		}
	}
	return "{}", 0, n > 0
}

func appLabel(d *models.Document) string {
	if d.Application == nil {
		return ""
	}
	return fmt.Sprintf("%s (%s)", d.Application.AppName, d.Application.AppCode)
}

func moduleLabel(m string) string {
	if m == "EVIDENCE" {
		return "Evidence"
	}
	return "Aplikasi"
}

func teamLabel(d *models.Document) string {
	if d.VisibilityScope == "ALL_TIM" && d.OwnerTeam == nil {
		return "Semua Tim"
	}
	if d.OwnerTeam != nil {
		if d.OwnerBranch != nil {
			return d.OwnerTeam.TeamName + " • " + d.OwnerBranch.BranchName
		}
		return d.OwnerTeam.TeamName
	}
	if d.UploadedByTeamLabel != "" {
		return d.UploadedByTeamLabel
	}
	return "-"
}

// ─── search_documents ────────────────────────────────────────────────────────

type searchItem struct {
	Ref         string  `json:"ref"`
	DocRef      string  `json:"doc_ref"`
	Title       string  `json:"title"`
	Category    string  `json:"category"`
	Version     string  `json:"version"`
	Application string  `json:"application,omitempty"`
	Module      string  `json:"module"`
	Section     int     `json:"section"`
	Score       float64 `json:"score"`
	Text        string  `json:"text,omitempty"`
	Summary     string  `json:"summary,omitempty"`
}

func runSearchDocumentsTool(ctx context.Context, a *agentRun, args map[string]any, st *AgentStep) toolResult {
	query := argString(args, "query")
	f, ferr := filterFromArgs(args)
	if ferr != "" {
		return errorResult(ferr)
	}
	if query == "" && f.empty() {
		return errorResult("query wajib diisi")
	}
	// app_code yang tidak cocok dengan aplikasi mana pun dalam cakupan (mis. nama dokumen Evidence) dipakai sebagai
	// kata kunci, bukan filter, agar pencarian tidak kosong karena salah tebak filter
	appNote := ""
	if f.AppCode != "" && !a.anyDocMatches(docFilter{AppCode: f.AppCode}) {
		appNote = fmt.Sprintf("app_code %q tidak cocok dengan aplikasi mana pun dalam cakupan user; dipakai sebagai kata kunci.", f.AppCode)
		query = strings.TrimSpace(query + " " + f.AppCode)
		f.AppCode = ""
	}
	topK := argInt(args, "top_k", a.rerankCfg.TopN, 1, MaxRerankTopN)
	candidates := a.rerankCfg.Candidates
	if candidates <= 0 {
		candidates = DefaultRerankCandidates
	}
	st.Queries = []string{query}
	st.Tool.Phase = fmt.Sprintf("Mengambil hingga %d kandidat dari indeks kata & metadata…", candidates)
	a.tr.update(st)

	cands, fresh := a.searchFiltered(query, f, candidates)
	ranked, scores, info := a.rerankCandidates(ctx, query, cands, topK, st)

	build := func(n int) any {
		summarized := map[string]bool{}
		items := make([]searchItem, 0, n)
		for _, ev := range ranked[:n] {
			it := searchItem{Ref: ev.Ref, DocRef: a.docRef(ev.Doc), Title: ev.Doc.Title, Category: ev.Doc.DocCategory,
				Version: ev.Doc.VersionLabel, Application: appLabel(ev.Doc), Module: moduleLabel(ev.Doc.ModuleSource),
				Section: ev.ChunkIndex + 1, Score: math.Round(scores[ev]*100) / 100}
			if a.cfg.ShareDocContent {
				it.Text = clip(oneLine(ev.Text), toolSnippetRunes)
			} else if !summarized[ev.Doc.ID] {
				summarized[ev.Doc.ID] = true
				it.Summary = clip(oneLine(ev.Doc.AISummary), toolSummaryRunes)
			}
			items = append(items, it)
		}
		payload := map[string]any{"query": query, "results": items, "total_candidates": len(cands)}
		if fm := f.asMap(); len(fm) > 0 {
			payload["filters"] = fm
		}
		if info.Method == "model" {
			payload["reranked_by"] = info.Provider + ":" + info.Model
		}
		if len(items) == 0 {
			payload["note"] = "Tidak ada dokumen dalam cakupan akses user yang cocok. Coba kueri lain (sinonim/singkatan), longgarkan filter, atau pakai list_documents."
		}
		if appNote != "" {
			payload["filter_note"] = appNote
		}
		if !a.cfg.ShareDocContent {
			payload["content_policy"] = "Isi file tidak dibagikan ke AI eksternal; hanya metadata & ringkasan pengunggah."
		}
		return payload
	}
	out, shown, truncated := fitJSON(build, len(ranked))

	res := toolResult{Output: out, Count: shown, Fresh: fresh, Truncated: truncated, Rerank: info, Queries: st.Queries}
	focus := contentSearchTerms(query, a.known)
	keys := excerptKeys(append(focus, synonymExpansions(query, focus)...))
	prev := map[*evidence]int{}
	for i, ev := range cands {
		prev[ev] = i + 1
	}
	for _, ev := range ranked[:shown] {
		res.Results = append(res.Results, StepResult{Ref: ev.Ref, DocumentID: ev.Doc.ID, Title: ev.Doc.Title, Category: ev.Doc.DocCategory,
			Version: ev.Doc.VersionLabel, Section: ev.ChunkIndex + 1, Score: math.Round(scores[ev]*100) / 100,
			PrevRank: prev[ev], Selected: true, Snippet: makeExcerpt(ev.Text, keys, 160)})
	}
	method := "heuristik"
	if info.Method == "model" {
		method = fmt.Sprintf("%s:%s, %d ms", info.Provider, info.Model, info.LatencyMs)
	}
	switch {
	case len(cands) == 0:
		res.Summary = "Tidak ada kandidat yang cocok dalam cakupan akses user."
	default:
		res.Summary = fmt.Sprintf("%d kandidat dinilai ulang (%s); %d teratas dikirim ke model, %d temuan baru.", len(cands), method, shown, fresh)
	}
	if info.Fallback != "" {
		res.Summary += " Reranker model tidak dipakai: " + info.Fallback + "."
	}
	if appNote != "" {
		res.Summary += " " + appNote
	}
	return res
}

// anyDocMatches: ada dokumen dalam cakupan user yang memenuhi filter.
func (a *agentRun) anyDocMatches(f docFilter) bool {
	for i := range a.docs {
		if f.matches(&a.docs[i]) {
			return true
		}
	}
	return false
}

// rerankCandidates menilai ulang kandidat satu pencarian: heuristik (RRF, BM25, cakupan & kedekatan kata, metadata,
// versi terbaru) selalu dihitung, lalu digabung dengan skor model reranker bila aktif. Maksimal maxChunksPerDocResult
// potongan per dokumen agar hasil beragam.
func (a *agentRun) rerankCandidates(ctx context.Context, query string, cands []*evidence, topN int, st *AgentStep) ([]*evidence, map[*evidence]float64, *RerankInfo) {
	info := &RerankInfo{Method: "heuristic", Candidates: len(cands), ContentShared: a.cfg.ShareDocContent, Weights: heuristicWeightLabel}
	scores := map[*evidence]float64{}
	if len(cands) == 0 {
		return nil, scores, info
	}
	if a.latest == nil {
		a.latest = latestSectionDocs(a.docs)
	}
	focus := contentSearchTerms(query, a.known)
	focus = append(focus, synonymExpansions(query, focus)...)
	heur := append([]*evidence(nil), cands...)
	rerankEvidence(heur, focus, a.latest, a.metaSets)
	for _, ev := range heur {
		scores[ev] = ev.Score
	}

	switch {
	case a.reranker != nil:
		st.Tool.Phase = fmt.Sprintf("Menilai ulang %d kandidat dengan %s…", len(cands), a.reranker.Name())
		a.tr.update(st)
		texts := make([]string, len(cands))
		for i, ev := range cands {
			texts[i] = rerankText(ev, a.cfg.ShareDocContent)
		}
		rctx, cancel := context.WithTimeout(ctx, rerankTimeout)
		start := time.Now()
		model, err := a.reranker.Rerank(rctx, query, texts, len(texts))
		cancel()
		info.LatencyMs = time.Since(start).Milliseconds()
		if name := strings.SplitN(a.reranker.Name(), ":", 2); len(name) == 2 {
			info.Provider, info.Model = name[0], name[1]
		}
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("[Agentic AI] reranker %s gagal, memakai heuristik: %v", a.reranker.Name(), err)
			}
			info.Fallback = describeRerankFailure(err)
			break
		}
		norm := normalizeRerankScores(model, len(cands))
		for i, ev := range cands {
			scores[ev] = rerankModelWeight*norm[i] + (1-rerankModelWeight)*scores[ev]
		}
		info.Method, info.Weights = "model", rerankWeightsLabel
	case a.rerankCfg.Provider != "" && a.rerankCfg.Provider != RerankProviderNone:
		info.Fallback = "konfigurasi reranker belum lengkap (base URL, model, atau API key kosong)"
	}

	ranked := append([]*evidence(nil), cands...)
	sort.SliceStable(ranked, func(i, j int) bool { return scores[ranked[i]] > scores[ranked[j]] })
	perDoc := map[string]int{}
	var out []*evidence
	for _, ev := range ranked {
		if len(out) >= topN {
			break
		}
		if perDoc[ev.Doc.ID] >= maxChunksPerDocResult {
			continue
		}
		perDoc[ev.Doc.ID]++
		out = append(out, ev)
	}
	info.Returned = len(out)
	return out, scores, info
}

// describeRerankFailure menjelaskan kegagalan reranker untuk log langkah.
func describeRerankFailure(err error) string {
	var llmErr *LLMError
	if errors.As(err, &llmErr) {
		switch llmErr.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "API key reranker ditolak"
		case http.StatusTooManyRequests:
			return "kuota reranker habis"
		case http.StatusNotFound:
			return "model atau alamat reranker tidak ditemukan"
		}
		return fmt.Sprintf("reranker membalas status %d", llmErr.Status)
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return fmt.Sprintf("reranker tidak merespons dalam %d detik", int(rerankTimeout.Seconds()))
	}
	return "reranker tidak dapat dihubungi"
}

// ─── read_document ───────────────────────────────────────────────────────────

type readSection struct {
	Section int    `json:"section"`
	Text    string `json:"text"`
}

type readDoc struct {
	Ref         string        `json:"ref"`
	DocRef      string        `json:"doc_ref"`
	Title       string        `json:"title"`
	Category    string        `json:"category"`
	Version     string        `json:"version"`
	Application string        `json:"application,omitempty"`
	Sections    []readSection `json:"sections,omitempty"`
	Summary     string        `json:"summary,omitempty"`
	Error       string        `json:"error,omitempty"`
}

func splitRefs(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		p = strings.ToUpper(strings.Trim(strings.TrimSpace(p), "[]"))
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func runReadDocumentTool(ctx context.Context, a *agentRun, args map[string]any, st *AgentStep) toolResult {
	refs := splitRefs(argString(args, "ref"))
	if len(refs) == 0 {
		return errorResult("ref wajib diisi, mis. E2 atau D1")
	}
	if len(refs) > agentMaxReads {
		refs = refs[:agentMaxReads]
	}
	st.Tool.Phase = "Memuat bagian dokumen beserta bagian sebelum & sesudahnya…"
	a.tr.update(st)

	var docs []readDoc
	res := toolResult{}
	for _, ref := range refs {
		var evs []*evidence
		var doc *models.Document
		switch {
		case a.byRef[ref] != nil:
			ev := a.byRef[ref]
			if a.loadNeighbors(ev) {
				res.Fresh++
			}
			evs, doc = []*evidence{ev}, ev.Doc
		case a.byDRef[ref] != nil:
			doc = a.byDRef[ref]
			first := !a.readDocs[doc.ID]
			for _, h := range loadLeadingChunks(a.db, []string{doc.ID})[doc.ID] {
				if ev, _ := a.addEvidence(h, true); ev != nil {
					evs = append(evs, ev)
				}
			}
			a.readDocs[doc.ID] = true
			a.tr.trace.DocsRead = len(a.readDocs)
			if first {
				res.Fresh++
			}
		default:
			docs = append(docs, readDoc{Ref: ref, Error: "ref tidak dikenal; gunakan ref dari hasil tool sebelumnya pada jawaban ini"})
			continue
		}
		rd := readDoc{Ref: ref, DocRef: a.docRef(doc), Title: doc.Title, Category: doc.DocCategory, Version: doc.VersionLabel, Application: appLabel(doc)}
		var chunks []chunkHit
		for _, ev := range evs {
			chunks = append(chunks, evidenceChunks(ev)...)
		}
		sort.Slice(chunks, func(i, j int) bool { return chunks[i].ChunkIndex < chunks[j].ChunkIndex })
		seen := map[int]bool{}
		if a.cfg.ShareDocContent {
			for _, c := range chunks {
				if !seen[c.ChunkIndex] {
					seen[c.ChunkIndex] = true
					rd.Sections = append(rd.Sections, readSection{Section: c.ChunkIndex + 1, Text: clip(oneLine(c.Text), toolReadSectionRunes)})
				}
			}
		} else {
			rd.Summary = clip(oneLine(doc.AISummary), toolSummaryRunes)
		}
		docs = append(docs, rd)
		res.Results = append(res.Results, StepResult{Ref: ref, DocumentID: doc.ID, Title: doc.Title, Category: doc.DocCategory,
			Version: doc.VersionLabel, Section: firstSection(evs), Selected: true})
	}
	// Bila terlalu panjang, jumlah bagian per dokumen dipangkas (bukan dokumennya) agar model tetap tahu semua ref
	build := func(n int) any {
		trimmed := make([]readDoc, len(docs))
		copy(trimmed, docs)
		for i := range trimmed {
			if len(trimmed[i].Sections) > max(1, n) {
				trimmed[i].Sections = trimmed[i].Sections[:max(1, n)]
			}
		}
		payload := map[string]any{"documents": trimmed}
		if !a.cfg.ShareDocContent {
			payload["content_policy"] = "Isi file tidak dibagikan ke AI eksternal; sarankan user membuka dokumen lewat tombol Lihat untuk rinciannya."
		}
		return payload
	}
	maxSections := 0
	for _, d := range docs {
		maxSections = max(maxSections, len(d.Sections))
	}
	out, _, truncated := fitJSON(build, maxSections)
	res.Output, res.Truncated, res.Count = out, truncated, len(res.Results)
	res.Summary = fmt.Sprintf("Membaca %d ref (%d dokumen dibaca sepanjang jawaban ini).", len(refs), len(a.readDocs))
	if len(res.Results) == 0 {
		res.Err = "Ref yang diminta tidak dikenal."
		res.Summary = res.Err
	}
	return res
}

func firstSection(evs []*evidence) int {
	if len(evs) == 0 {
		return 0
	}
	return evs[0].ChunkIndex + 1
}

// ─── list_documents ──────────────────────────────────────────────────────────

type listItem struct {
	Ref         string `json:"ref"`
	Title       string `json:"title"`
	Category    string `json:"category"`
	Version     string `json:"version"`
	Application string `json:"application,omitempty"`
	Module      string `json:"module"`
	Team        string `json:"team"`
	UploadedBy  string `json:"uploaded_by"`
	UploadedAt  string `json:"uploaded_at"`
	ValidUntil  string `json:"valid_until,omitempty"`
	Summary     string `json:"summary,omitempty"`
}

func runListDocumentsTool(ctx context.Context, a *agentRun, args map[string]any, st *AgentStep) toolResult {
	f, ferr := filterFromArgs(args)
	if ferr != "" {
		return errorResult(ferr)
	}
	contains := strings.ToLower(argString(args, "title_contains"))
	sortBy := strings.ToLower(argString(args, "sort"))
	if sortBy != "" && !inList(sortBy, []string{"newest", "oldest", "title"}) {
		return errorResult("sort harus newest, oldest, atau title")
	}
	var after time.Time
	if s := argString(args, "uploaded_after"); s != "" {
		t, err := time.ParseInLocation("2006-01-02", s, wib)
		if err != nil {
			return errorResult("uploaded_after harus berformat YYYY-MM-DD")
		}
		after = t
	}
	limit := argInt(args, "limit", listDocumentsDefault, 1, listDocumentsMax)

	var matched []*models.Document
	for i := range a.docs {
		d := &a.docs[i]
		if !f.matches(d) || (!after.IsZero() && d.UploadedAt.Before(after)) {
			continue
		}
		if contains != "" && !strings.Contains(strings.ToLower(d.Title+" "+d.AISummary), contains) {
			continue
		}
		matched = append(matched, d)
	}
	switch sortBy {
	case "oldest":
		sort.SliceStable(matched, func(i, j int) bool { return matched[i].UploadedAt.Before(matched[j].UploadedAt) })
	case "title":
		sort.SliceStable(matched, func(i, j int) bool { return strings.ToLower(matched[i].Title) < strings.ToLower(matched[j].Title) })
	default: // a.docs sudah urut terbaru
	}
	if len(matched) > limit {
		matched = matched[:limit]
	}
	items := make([]listItem, len(matched))
	for i, d := range matched {
		items[i] = listItem{Ref: a.docRef(d), Title: d.Title, Category: d.DocCategory, Version: d.VersionLabel, Application: appLabel(d),
			Module: moduleLabel(d.ModuleSource), Team: teamLabel(d), UploadedBy: fmt.Sprintf("%s (%s)", d.UploadedByName, d.UploadedByBadge),
			UploadedAt: d.UploadedAt.In(wib).Format("2006-01-02 15:04") + " WIB", ValidUntil: d.ValidUntil,
			Summary: clip(oneLine(d.AISummary), toolSummaryRunes)}
	}
	total := 0
	for i := range a.docs {
		if f.matches(&a.docs[i]) {
			total++
		}
	}
	build := func(n int) any {
		payload := map[string]any{"total_in_scope_matching_filters": total, "items": items[:n]}
		if fm := f.asMap(); len(fm) > 0 {
			payload["filters"] = fm
		}
		if n == 0 && len(items) == 0 {
			payload["note"] = "Tidak ada dokumen dalam cakupan akses user yang memenuhi filter."
		}
		return payload
	}
	out, shown, truncated := fitJSON(build, len(items))
	res := toolResult{Output: out, Count: shown, Truncated: truncated, Fresh: shown}
	for _, it := range items[:shown] {
		d := a.byDRef[it.Ref]
		res.Results = append(res.Results, StepResult{Ref: it.Ref, DocumentID: d.ID, Title: d.Title, Category: d.DocCategory, Version: d.VersionLabel,
			Snippet: it.UploadedAt + " · " + it.UploadedBy})
	}
	res.Summary = fmt.Sprintf("%d dokumen memenuhi filter; %d ditampilkan ke model.", total, shown)
	return res
}

// ─── get_application ─────────────────────────────────────────────────────────

type appVersion struct {
	Version       string     `json:"version"`
	Title         string     `json:"title"`
	ReleaseDate   string     `json:"release_date,omitempty"`
	InitialRelase bool       `json:"initial_release"`
	Description   string     `json:"description,omitempty"`
	Documents     []listItem `json:"documents"`
	MissingCore   []string   `json:"missing_core_categories,omitempty"`
}

// scopedApplications mencari aplikasi dalam folder yang boleh diakses user (aturan menu Aplikasi). Tanpa database
// (unit test), aplikasi diambil dari dokumen dalam cakupan.
func (a *agentRun) scopedApplications(key string, limit int) []models.Application {
	if a.db == nil {
		seen := map[string]bool{}
		var out []models.Application
		for i := range a.docs {
			app := a.docs[i].Application
			if app != nil && !seen[app.ID] && (key == "" || appMatches(app, key)) && len(out) < limit {
				seen[app.ID] = true
				cp := *app
				if a.docs[i].Folder != nil {
					cp.Folder = a.docs[i].Folder
				}
				out = append(out, cp)
			}
		}
		return out
	}
	q := a.db.Preload("Folder.AssignedTeam").Preload("Folder.AssignedBranch").
		Preload("Sections", func(db *gorm.DB) *gorm.DB { return db.Order("sort_order ASC") }).
		Where("folder_id IN (?)", ApplyFolderScope(a.db.Model(&models.AppFolder{}), a.user).Select("id"))
	if key != "" {
		q = q.Where("app_code = ? OR app_name LIKE ?", key, "%"+escapeLike(key)+"%")
	}
	var apps []models.Application
	q.Order("app_name").Limit(limit).Find(&apps)
	return apps
}

func runGetApplicationTool(ctx context.Context, a *agentRun, args map[string]any, st *AgentStep) toolResult {
	key := argString(args, "app")
	if key == "" {
		key = argString(args, "app_code")
	}
	if key == "" {
		return errorResult("app wajib diisi (kode aplikasi atau potongan namanya)")
	}
	apps := a.scopedApplications(key, 5)
	var app *models.Application
	for i := range apps {
		if strings.EqualFold(apps[i].AppCode, key) {
			app = &apps[i]
		}
	}
	if app == nil && len(apps) == 1 {
		app = &apps[0]
	}
	if app == nil {
		var names []string
		list := apps
		if len(list) == 0 {
			list = a.scopedApplications("", 20)
		}
		for _, x := range list {
			names = append(names, fmt.Sprintf("%s (%s)", x.AppName, x.AppCode))
		}
		msg := "Aplikasi tidak ditemukan dalam cakupan akses user."
		if len(apps) > 1 {
			msg = "Beberapa aplikasi cocok; panggil lagi dengan kode yang tepat."
		}
		b, _ := json.Marshal(map[string]any{"found": false, "message": msg, "applications_in_scope": names})
		return toolResult{Output: string(b), Summary: msg}
	}

	sections := app.Sections
	if len(sections) == 0 {
		seen := map[string]bool{}
		for i := range a.docs {
			if s := a.docs[i].Section; s != nil && s.ApplicationID == app.ID && !seen[s.ID] {
				seen[s.ID] = true
				sections = append(sections, *s)
			}
		}
		sort.SliceStable(sections, func(i, j int) bool { return sections[i].SortOrder < sections[j].SortOrder })
	}
	res := toolResult{}
	versions := make([]appVersion, 0, len(sections))
	for _, s := range sections {
		v := appVersion{Version: s.VersionLabel, Title: s.SectionTitle, ReleaseDate: s.ReleaseDate, InitialRelase: s.IsInitialRelease,
			Description: clip(s.ChangeDescription, toolSummaryRunes), Documents: []listItem{}}
		present := map[string]bool{}
		for i := range a.docs {
			d := &a.docs[i]
			if d.SectionID == nil || *d.SectionID != s.ID {
				continue
			}
			present[d.DocCategory] = true
			v.Documents = append(v.Documents, listItem{Ref: a.docRef(d), Title: d.Title, Category: d.DocCategory, Version: d.VersionLabel,
				Module: moduleLabel(d.ModuleSource), UploadedBy: d.UploadedByName, UploadedAt: d.UploadedAt.In(wib).Format("2006-01-02")})
			res.Results = append(res.Results, StepResult{Ref: a.docRef(d), DocumentID: d.ID, Title: d.Title, Category: d.DocCategory, Version: d.VersionLabel})
		}
		core := coreChangeCategories
		if s.IsInitialRelease {
			core = coreInitialCategories
		}
		for _, c := range core {
			if !present[c] {
				v.MissingCore = append(v.MissingCore, c)
			}
		}
		versions = append(versions, v)
	}
	folder, team := "", ""
	if app.Folder != nil {
		folder = app.Folder.FolderName
		if app.Folder.AssignedTeam != nil {
			team = app.Folder.AssignedTeam.TeamName
			if app.Folder.AssignedBranch != nil {
				team += " • " + app.Folder.AssignedBranch.BranchName
			}
		}
	}
	build := func(n int) any {
		vs := versions
		if n < len(versions) {
			vs = versions[len(versions)-n:] // versi terbaru diutamakan
		}
		return map[string]any{
			"found": true,
			"application": map[string]string{"code": app.AppCode, "name": app.AppName, "status": app.Status,
				"description": clip(app.Description, toolSummaryRunes), "folder": folder, "team": team},
			"versions":       vs,
			"total_versions": len(versions),
			"note":           "missing_core_categories adalah kategori dokumen inti yang belum ada (panduan PANDA: versi awal PRD, FSD, UAT, BAST; versi perubahan PRD, UAT, BAST), bukan aturan wajib.",
		}
	}
	out, _, truncated := fitJSON(build, len(versions))
	res.Output, res.Truncated, res.Count, res.Fresh = out, truncated, len(res.Results), len(res.Results)
	res.Summary = fmt.Sprintf("%s (%s): %d versi, %d dokumen.", app.AppName, app.AppCode, len(versions), len(res.Results))
	return res
}

// ─── get_repository_stats ────────────────────────────────────────────────────

func runRepositoryStatsTool(ctx context.Context, a *agentRun, args map[string]any, st *AgentStep) toolResult {
	groupBy := strings.ToLower(argString(args, "group_by"))
	if groupBy == "" {
		groupBy = "category"
	}
	if !inList(groupBy, []string{"category", "team", "module", "application"}) {
		return errorResult("group_by harus category, team, module, atau application")
	}
	counts := map[string]int{}
	for i := range a.docs {
		d := &a.docs[i]
		key := ""
		switch groupBy {
		case "category":
			key = d.DocCategory
		case "team":
			key = teamLabel(d)
		case "module":
			key = moduleLabel(d.ModuleSource)
		case "application":
			key = appLabel(d)
			if key == "" {
				key = "(Evidence tanpa aplikasi)"
			}
		}
		counts[key]++
	}
	type group struct {
		Key   string `json:"key"`
		Count int    `json:"count"`
	}
	groups := make([]group, 0, len(counts))
	for k, n := range counts {
		groups = append(groups, group{k, n})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}
		return groups[i].Key < groups[j].Key
	})
	build := func(n int) any {
		return map[string]any{"scope": scopeLabelFor(a.user), "total_documents": len(a.docs), "group_by": groupBy, "groups": groups[:n]}
	}
	out, shown, truncated := fitJSON(build, len(groups))
	return toolResult{Output: out, Count: shown, Truncated: truncated,
		Summary: fmt.Sprintf("%d dokumen dalam cakupan, %d kelompok per %s.", len(a.docs), len(groups), groupBy)}
}
