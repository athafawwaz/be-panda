package service

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"panda-backend/internal/models"
)

const (
	agentSearchLimit   = 8 // temuan per pencarian yang diteruskan ke agen
	agentMaxReads      = 3 // potongan per aksi read
	agentMetaOnlyDocs  = 3 // dokumen yang cocok dari judul/ringkasan saja per pencarian
	agentMaxFinishRefs = 8
	builtinMaxQueries  = 6
	agentAnswerTimeout = 120 * time.Second

	// Mode otomatis (tanpa batas langkah): agen berhenti sendiri saat bukti cukup. Rem pengaman di bawah hanya
	// mencegah agen berputar tanpa hasil atau menghabiskan kuota, bukan membatasi penelusuran yang produktif.
	agentSafetyMaxSteps      = 40              // plafon darurat keputusan agen
	agentResearchBudget      = 4 * time.Minute // batas waktu penelusuran sebelum jawaban disusun
	builtinSafetyMaxSearches = 15              // batas pencarian mode tanpa LLM
	builtinExpansionRounds   = 3               // putaran perluasan dari temuan teratas (mode tanpa LLM)
)

// AgentEmitter menerima event progres jawaban (langkah, potongan jawaban, token) untuk ditampilkan langsung.
// nil = tanpa streaming (endpoint POST /ai/chat biasa).
type AgentEmitter func(event string, data any)

// ─── Jejak langkah (ditampilkan langsung & disimpan di riwayat chat) ─────────

// AgentStep adalah satu langkah AI: menimbang (think), memanggil tool, mencari, membaca, menilai ulang, atau menyusun
// jawaban. Langkah dikirim ulang (event "step" dengan id yang sama) setiap kali isinya berubah, sehingga log di layar
// selalu mutakhir; versi terakhirnya disimpan di riwayat chat.
type AgentStep struct {
	ID         int           `json:"id"`
	Kind       string        `json:"kind"` // think | tool | search | read | rerank | answer
	Title      string        `json:"title"`
	Detail     string        `json:"detail,omitempty"` // alasan/pemikiran singkat agen atau ringkasan hasil langkah
	Queries    []string      `json:"queries,omitempty"`
	Results    []StepResult  `json:"results,omitempty"`
	Status     string        `json:"status"`     // running | done | error
	StartedMs  int64         `json:"started_ms"` // milidetik sejak pertanyaan diterima
	DurationMs int64         `json:"duration_ms"`
	Tokens     int           `json:"tokens,omitempty"`
	Round      int           `json:"round,omitempty"`  // putaran agent loop (tool calling)
	Model      string        `json:"model,omitempty"`  // model LLM yang dipanggil pada langkah ini
	Finish     string        `json:"finish,omitempty"` // finish_reason dari model (stop | tool_calls | length)
	Tool       *ToolCallInfo `json:"tool,omitempty"`   // detail panggilan tool (kind = tool)
	Rerank     *RerankInfo   `json:"rerank,omitempty"` // detail penilaian ulang (kind = rerank, atau di dalam tool pencarian)
}

// ToolCallInfo adalah detail satu panggilan tool: argumen dari model, tahap yang sedang berjalan, dan hasil yang
// dikirim balik ke model.
type ToolCallInfo struct {
	Name        string         `json:"name"`
	CallID      string         `json:"call_id,omitempty"`
	Arguments   map[string]any `json:"arguments,omitempty"`
	RawArgs     string         `json:"raw_arguments,omitempty"` // argumen mentah bila bukan JSON valid
	Phase       string         `json:"phase,omitempty"`         // tahap yang sedang berjalan (diperbarui langsung)
	ResultCount int            `json:"result_count"`
	ResultChars int            `json:"result_chars"`        // panjang hasil (JSON) yang dikirim ke model
	Truncated   bool           `json:"truncated,omitempty"` // hasil dipangkas agar muat batas
	Output      string         `json:"output,omitempty"`    // cuplikan hasil persis yang dikirim ke model
	Error       string         `json:"error,omitempty"`
}

// RerankInfo menjelaskan cara temuan dinilai ulang.
type RerankInfo struct {
	Method        string `json:"method"` // model | heuristic
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	Candidates    int    `json:"candidates"`
	Returned      int    `json:"returned"`
	LatencyMs     int64  `json:"latency_ms"`
	ContentShared bool   `json:"content_shared"`     // isi potongan dikirim ke reranker (false = judul & ringkasan saja)
	Weights       string `json:"weights,omitempty"`  // komposisi skor akhir
	Fallback      string `json:"fallback,omitempty"` // alasan beralih ke heuristik
}

// StepResult adalah satu temuan yang ditampilkan pada langkah pencarian, baca, atau rerank.
type StepResult struct {
	Ref        string  `json:"ref,omitempty"`
	DocumentID string  `json:"document_id"`
	Title      string  `json:"title"`
	Category   string  `json:"category"`
	Version    string  `json:"version"`
	Section    int     `json:"section,omitempty"` // bagian ke-n (urutan potongan isi)
	Score      float64 `json:"score"`             // 0–1
	PrevRank   int     `json:"prev_rank,omitempty"`
	Selected   bool    `json:"selected,omitempty"` // dipakai sebagai bukti jawaban
	Snippet    string  `json:"snippet,omitempty"`
}

// AgentTrace merangkum seluruh langkah, token, dan durasi satu jawaban.
type AgentTrace struct {
	Thinking       bool        `json:"thinking"`
	Steps          []AgentStep `json:"steps"`
	Usage          LLMUsage    `json:"usage"`
	DurationMs     int64       `json:"duration_ms"`
	Searches       int         `json:"searches"`
	Findings       int         `json:"findings"`
	DocsRead       int         `json:"docs_read"`
	DocContentUsed *bool       `json:"doc_content_used,omitempty"`
}

type tracer struct {
	emit  AgentEmitter
	trace AgentTrace
	steps []*AgentStep
	start time.Time
}

func newTracer(thinking bool, emit AgentEmitter) *tracer {
	return &tracer{emit: emit, trace: AgentTrace{Thinking: thinking}, start: time.Now()}
}

func (t *tracer) send(event string, data any) {
	if t.emit != nil {
		t.emit(event, data)
	}
}

func (t *tracer) begin(kind, title string) *AgentStep {
	s := &AgentStep{ID: len(t.steps) + 1, Kind: kind, Title: title, Status: "running", StartedMs: time.Since(t.start).Milliseconds()}
	t.steps = append(t.steps, s)
	t.send("step", *s)
	return s
}

// update mengirim ulang langkah yang isinya berubah (mis. argumen tool terbaca, tahap rerank, hasil masuk).
func (t *tracer) update(s *AgentStep) { t.send("step", *s) }

func (t *tracer) end(s *AgentStep) {
	s.Status = "done"
	s.DurationMs = time.Since(t.start).Milliseconds() - s.StartedMs
	t.send("step", *s)
}

func (t *tracer) fail(s *AgentStep, detail string) {
	s.Status = "error"
	s.Detail = detail
	s.DurationMs = time.Since(t.start).Milliseconds() - s.StartedMs
	t.send("step", *s)
}

func (t *tracer) addUsage(s *AgentStep, u LLMUsage) {
	if u.Calls == 0 {
		return
	}
	t.trace.Usage.Add(u)
	if s != nil {
		s.Tokens += u.TotalTokens
	}
	t.send("usage", t.trace.Usage)
}

func (t *tracer) delta(text string) {
	if text != "" {
		t.send("answer_delta", map[string]string{"text": text})
	}
}

// resetAnswer membuang teks yang sudah mengalir tetapi bukan bagian jawaban akhir.
func (t *tracer) resetAnswer() { t.send("answer_reset", map[string]bool{"reset": true}) }

func (t *tracer) finish(resp *AIChatResponse) *AIChatResponse {
	t.trace.DurationMs = time.Since(t.start).Milliseconds()
	t.trace.Steps = make([]AgentStep, len(t.steps))
	for i, s := range t.steps {
		t.trace.Steps[i] = *s
	}
	trace := t.trace
	resp.Trace = &trace
	if resp.QueriedAt.IsZero() {
		resp.QueriedAt = time.Now()
	}
	if resp.MatchedDocuments == nil {
		resp.MatchedDocuments = []AIDocumentCitation{}
	}
	return resp
}

// ─── Agen riset (mode Thinking) ──────────────────────────────────────────────

type agentRun struct {
	db       *gorm.DB
	user     models.User
	req      AIChatRequest
	cfg      LLMConfig
	tr       *tracer
	docs     []models.Document
	docByID  map[string]*models.Document
	metaSets map[string]map[string]bool
	known    map[string]bool
	keys     []string // kata untuk membuat cuplikan temuan
	pool     []*evidence
	byChunk  map[string]*evidence
	byRef    map[string]*evidence
	ran      map[string]bool
	readDocs map[string]bool
	// Tool calling: bagian yang sudah dibaca, ref tingkat dokumen (D#), dan reranker untuk tool pencarian
	readChunks      map[string]bool
	dRefs           map[string]string
	byDRef          map[string]*models.Document
	reranker        Reranker
	rerankCfg       RerankConfig
	latest          map[string]bool // dokumen dari versi terbaru (dihitung saat pertama dibutuhkan)
	shownRefs       map[string]bool
	successfulTools int
	docContentUsed  bool
}

func newAgentRun(db *gorm.DB, user models.User, req AIChatRequest, cfg LLMConfig, tr *tracer) (*agentRun, error) {
	docs, err := loadCandidateDocs(db, user, req.FilterModule)
	if err != nil {
		return nil, err
	}
	return newAgentRunWithDocs(db, user, req, cfg, tr, docs), nil
}

// newAgentRunWithDocs membangun agen dari dokumen yang SUDAH dibatasi cakupan akses user (loadCandidateDocs).
func newAgentRunWithDocs(db *gorm.DB, user models.User, req AIChatRequest, cfg LLMConfig, tr *tracer, docs []models.Document) *agentRun {
	a := &agentRun{db: db, user: user, req: req, cfg: cfg, tr: tr, docs: docs,
		docByID: map[string]*models.Document{}, metaSets: map[string]map[string]bool{},
		byChunk: map[string]*evidence{}, byRef: map[string]*evidence{}, ran: map[string]bool{}, readDocs: map[string]bool{},
		readChunks: map[string]bool{}, dRefs: map[string]string{}, byDRef: map[string]*models.Document{}, shownRefs: map[string]bool{}}
	for i := range docs {
		a.docByID[docs[i].ID] = &docs[i]
		a.metaSets[docs[i].ID] = metadataTermSet(docs[i])
	}
	a.known = knownSearchWords(docs)
	a.rerankCfg = RerankConfig{Provider: RerankProviderNone, Candidates: DefaultRerankCandidates, TopN: DefaultRerankTopN}
	return a
}

func (a *agentRun) scopeIDs() *gorm.DB {
	return scopedDocuments(a.db, a.user, a.req.FilterModule).Select("id")
}

func normalizeQuery(q string) string {
	return strings.Join(splitWords(strings.ToLower(q)), " ")
}

// docFilter membatasi pencarian tool ke kategori, aplikasi, modul, atau versi tertentu (kosong = tanpa batas).
type docFilter struct {
	Category string
	AppCode  string
	Module   string
	Version  string
}

func (f docFilter) empty() bool {
	return f.Category == "" && f.AppCode == "" && f.Module == "" && f.Version == ""
}

// versionVariants: "v1.1", "1.1", dan "V1.1" dianggap versi yang sama.
func versionVariants(v string) []string {
	core := strings.TrimLeft(strings.TrimSpace(v), "vV ")
	return []string{"v" + core, "V" + core, core}
}

func sameVersion(a, b string) bool {
	return strings.EqualFold(strings.TrimLeft(strings.TrimSpace(a), "vV "), strings.TrimLeft(strings.TrimSpace(b), "vV "))
}

// appMatches: kode aplikasi persis, atau potongan nama aplikasi (tanpa membedakan huruf besar).
func appMatches(app *models.Application, query string) bool {
	if app == nil || strings.TrimSpace(query) == "" {
		return false
	}
	q := strings.ToLower(strings.TrimSpace(query))
	return strings.EqualFold(app.AppCode, q) || strings.Contains(strings.ToLower(app.AppName), q)
}

// matches adalah versi in-memory dari filteredScope.
func (f docFilter) matches(d *models.Document) bool {
	if f.Category != "" && !strings.EqualFold(d.DocCategory, f.Category) {
		return false
	}
	if f.Module != "" && !strings.EqualFold(d.ModuleSource, f.Module) {
		return false
	}
	if f.Version != "" && !sameVersion(d.VersionLabel, f.Version) {
		return false
	}
	if f.AppCode != "" && !appMatches(d.Application, f.AppCode) {
		return false
	}
	return true
}

// filteredScope: subquery id dokumen dalam cakupan user yang juga memenuhi filter. Isolasi tim tetap berasal dari
// scopedDocuments; filter hanya mempersempit.
func (a *agentRun) filteredScope(f docFilter) func() *gorm.DB {
	if f.empty() {
		return a.scopeIDs
	}
	return func() *gorm.DB {
		q := scopedDocuments(a.db, a.user, a.req.FilterModule)
		if f.Module != "" {
			q = q.Where("module_source = ?", strings.ToUpper(f.Module))
		}
		if f.Category != "" {
			q = q.Where("doc_category = ?", strings.ToUpper(f.Category))
		}
		if f.Version != "" {
			q = q.Where("version_label IN ?", versionVariants(f.Version))
		}
		if f.AppCode != "" {
			q = q.Where("application_id IN (?)", a.db.Model(&models.Application{}).Select("id").
				Where("LOWER(app_code) = LOWER(?) OR app_name ILIKE ?", f.AppCode, "%"+escapeLike(f.AppCode)+"%"))
		}
		return q.Select("id")
	}
}

// search menjalankan satu pencarian (indeks kata + sinonim + kecocokan judul/ringkasan) dan menambahkan temuannya
// ke pool. Satu potongan yang ditemukan beberapa query mendapat skor RRF lebih tinggi.
func (a *agentRun) search(query string) (found []*evidence, fresh int, err error) {
	return a.searchFiltered(query, docFilter{}, agentSearchLimit)
}

// searchFiltered seperti search, dengan filter dokumen dan jumlah temuan maksimal (kandidat untuk reranker).
// Kueri tanpa kata kunci tetapi dengan filter (mis. "dokumen UAT terbaru") mengembalikan bagian awal dokumen terbaru
// yang memenuhi filter.
func (a *agentRun) searchFiltered(query string, f docFilter, limit int) (found []*evidence, fresh int, err error) {
	terms := contentSearchTerms(query, a.known)
	terms = append(terms, synonymExpansions(query, terms)...)
	a.tr.trace.Searches++
	if len(terms) == 0 {
		if f.empty() {
			return nil, 0, nil
		}
		return a.latestByFilter(f, min(limit, agentMaxFinishRefs))
	}
	hits, _, err := retrieveRelevantChunks(a.db, a.filteredScope(f), terms)
	if err != nil {
		return nil, 0, err
	}

	var flat []chunkHit
	for _, hs := range hits {
		flat = append(flat, hs...)
	}
	sort.SliceStable(flat, func(i, j int) bool { return flat[i].Score > flat[j].Score })
	if len(flat) > limit {
		flat = flat[:limit]
	}

	// Dokumen yang judul/ringkasannya cocok tetapi isinya tidak memuat kata kunci tetap dipertimbangkan
	var primary []searchTerm
	for _, t := range terms {
		if t.primary() {
			primary = append(primary, t)
		}
	}
	var metaDocs []string
	for i := range a.docs {
		d := &a.docs[i]
		if len(metaDocs) >= agentMetaOnlyDocs || len(primary) == 0 {
			break
		}
		if _, ok := hits[d.ID]; ok || !f.matches(d) {
			continue
		}
		matched := 0
		for _, t := range primary {
			if setHasTerm(a.metaSets[d.ID], t.Term) {
				matched++
			}
		}
		if matched > 0 && matched*2 >= len(primary) {
			metaDocs = append(metaDocs, d.ID)
		}
	}
	metaChunks := map[string]bool{}
	leading, err := loadLeadingChunks(a.db, metaDocs)
	if err != nil {
		return nil, 0, err
	}
	for _, id := range metaDocs {
		if hs := leading[id]; len(hs) > 0 {
			flat = append(flat, hs[0])
			metaChunks[hs[0].ChunkID] = true
		}
	}

	byDoc := map[string][]chunkHit{}
	for _, h := range flat {
		byDoc[h.DocumentID] = append(byDoc[h.DocumentID], h)
	}
	if err := loadChunkTexts(a.db, byDoc); err != nil {
		return nil, 0, err
	}
	loaded := map[string]chunkHit{}
	for _, hs := range byDoc {
		for _, h := range hs {
			loaded[h.ChunkID] = h
		}
	}

	for _, h := range flat {
		h = loaded[h.ChunkID]
		ev, isNew := a.addEvidence(h, metaChunks[h.ChunkID])
		if ev == nil {
			continue
		}
		if isNew {
			fresh++
		}
		ev.BestBM25 = math.Max(ev.BestBM25, h.Score)
		found = append(found, ev)
	}
	addRanking(found)
	return found, fresh, nil
}

// latestByFilter mengembalikan bagian awal dokumen terbaru yang memenuhi filter sebagai temuan.
func (a *agentRun) latestByFilter(f docFilter, limit int) (found []*evidence, fresh int, err error) {
	var ids []string
	for i := range a.docs {
		if len(ids) >= limit {
			break
		}
		if f.matches(&a.docs[i]) {
			ids = append(ids, a.docs[i].ID)
		}
	}
	leading, err := loadLeadingChunks(a.db, ids)
	if err != nil {
		return nil, 0, err
	}
	for _, id := range ids {
		hs := leading[id]
		if len(hs) == 0 {
			continue
		}
		ev, isNew := a.addEvidence(hs[0], true)
		if ev == nil {
			continue
		}
		if isNew {
			fresh++
		}
		found = append(found, ev)
	}
	addRanking(found)
	return found, fresh, nil
}

// addEvidence mendaftarkan potongan ke pool (bila belum ada) dan mengembalikan temuannya.
func (a *agentRun) addEvidence(h chunkHit, fromMeta bool) (*evidence, bool) {
	if ev := a.byChunk[h.ChunkID]; ev != nil {
		return ev, false
	}
	doc := a.docByID[h.DocumentID]
	if doc == nil || h.ChunkID == "" {
		return nil, false
	}
	ev := &evidence{Ref: fmt.Sprintf("E%d", len(a.pool)+1), Doc: doc, ChunkID: h.ChunkID, ChunkIndex: h.ChunkIndex,
		Text: h.Text, FromMeta: fromMeta}
	a.pool = append(a.pool, ev)
	a.byChunk[h.ChunkID] = ev
	a.byRef[ev.Ref] = ev
	return ev, true
}

// loadNeighbors memuat potongan sebelum & sesudah temuan agar konteksnya utuh. true bila baru pertama kali dibaca.
func (a *agentRun) loadNeighbors(ev *evidence) (bool, error) {
	first := !a.readChunks[ev.ChunkID]
	var rows []models.DocumentChunk
	if err := a.db.Select("id", "chunk_index", "chunk_text").
		Where("document_id = ? AND chunk_index IN ?", ev.Doc.ID, []int{ev.ChunkIndex - 1, ev.ChunkIndex + 1}).
		Order("chunk_index").Find(&rows).Error; err != nil {
		return false, fmt.Errorf("load neighboring content chunks: %w", err)
	}
	a.readChunks[ev.ChunkID] = true
	ev.Neighbors = ev.Neighbors[:0]
	for _, r := range rows {
		ev.Neighbors = append(ev.Neighbors, chunkHit{ChunkID: r.ID, DocumentID: ev.Doc.ID, ChunkIndex: r.ChunkIndex, Text: r.ChunkText})
	}
	a.readDocs[ev.Doc.ID] = true
	a.tr.trace.DocsRead = len(a.readDocs)
	return first, nil
}

// docRef memberi ref tingkat dokumen (D1, D2, …) yang bisa dikutip dan dibaca seperti ref temuan.
func (a *agentRun) docRef(d *models.Document) string {
	if ref, ok := a.dRefs[d.ID]; ok {
		return ref
	}
	ref := fmt.Sprintf("D%d", len(a.dRefs)+1)
	a.dRefs[d.ID] = ref
	a.byDRef[ref] = d
	return ref
}

// docForRef mengembalikan dokumen untuk ref E# atau D# yang pernah diberikan pada jawaban ini (nil bila tidak dikenal).
// Ref hanya lahir dari dokumen dalam cakupan user, sehingga ref tebakan tidak bisa membuka dokumen tim lain.
func (a *agentRun) docForRef(ref string) *models.Document {
	ref = strings.ToUpper(strings.TrimSpace(ref))
	if ev := a.byRef[ref]; ev != nil {
		return ev.Doc
	}
	return a.byDRef[ref]
}

// searchStep menjalankan pencarian sebagai satu langkah yang tampil di layar. Query yang sudah pernah dijalankan
// dilewati (ok = false). fresh = jumlah temuan yang belum pernah ditemukan sebelumnya.
func (a *agentRun) searchStep(query string) (found []*evidence, fresh int, ok bool, err error) {
	query = strings.TrimSpace(query)
	key := normalizeQuery(query)
	if key == "" || a.ran[key] {
		return nil, 0, false, nil
	}
	a.ran[key] = true
	st := a.tr.begin("search", "Mencari dokumen")
	st.Queries = []string{query}
	a.tr.send("step", *st)
	found, fresh, err = a.search(query)
	if err != nil {
		a.tr.fail(st, "Pencarian dokumen gagal.")
		return nil, 0, true, err
	}
	st.Results = a.stepResults(found, 6, false)
	if len(found) == 0 {
		st.Detail = "Tidak ada temuan untuk kueri ini."
	} else {
		st.Detail = fmt.Sprintf("%d temuan, %d di antaranya baru.", len(found), fresh)
	}
	a.tr.end(st)
	return found, fresh, true, nil
}

func (a *agentRun) stepResults(evs []*evidence, limit int, finalScore bool) []StepResult {
	maxBM25 := 0.0
	for _, ev := range evs {
		maxBM25 = math.Max(maxBM25, ev.BestBM25)
	}
	var out []StepResult
	for _, ev := range evs {
		if len(out) >= limit {
			break
		}
		score := ev.Score
		if !finalScore {
			score = 0.3 // kecocokan dari judul/ringkasan saja
			if maxBM25 > 0 && ev.BestBM25 > 0 {
				score = math.Round(ev.BestBM25/maxBM25*100) / 100
			}
		}
		out = append(out, StepResult{Ref: ev.Ref, DocumentID: ev.Doc.ID, Title: ev.Doc.Title, Category: ev.Doc.DocCategory,
			Version: ev.Doc.VersionLabel, Section: ev.ChunkIndex + 1, Score: score, Snippet: makeExcerpt(ev.Text, a.keys, 160)})
	}
	return out
}

// read memuat bagian di sekitar temuan (potongan sebelum & sesudahnya) agar konteksnya utuh.
func (a *agentRun) read(refs []string) (string, error) {
	var evs []*evidence
	for _, r := range refs {
		if ev := a.byRef[strings.ToUpper(strings.TrimSpace(r))]; ev != nil && len(evs) < agentMaxReads {
			evs = append(evs, ev)
		}
	}
	if len(evs) == 0 {
		return "Ref yang diminta tidak dikenal; gunakan ref dari observasi (mis. E1).\n", nil
	}
	st := a.tr.begin("read", "Membaca bagian dokumen")
	var obs strings.Builder
	for _, ev := range evs {
		if _, err := a.loadNeighbors(ev); err != nil {
			a.tr.fail(st, "Pembacaan dokumen gagal.")
			return "", err
		}

		obs.WriteString(fmt.Sprintf("[%s] %s (%s) — bagian ke-%d beserta bagian di sekitarnya:\n", ev.Ref, ev.Doc.Title, ev.Doc.VersionLabel, ev.ChunkIndex+1))
		if a.cfg.ShareDocContent {
			for _, c := range evidenceChunks(ev) {
				obs.WriteString(fmt.Sprintf("  (bagian ke-%d) «%s»\n", c.ChunkIndex+1, clip(oneLine(c.Text), 700)))
			}
		} else {
			obs.WriteString("  (isi file tidak dibagikan ke AI eksternal sesuai kebijakan data; bagian ini tetap dipakai untuk jawaban)\n")
		}
	}
	st.Results = a.stepResults(evs, agentMaxReads, false)
	st.Detail = fmt.Sprintf("Membaca %d bagian beserta bagian sebelum & sesudahnya agar konteksnya utuh.", len(evs))
	a.tr.end(st)
	return obs.String(), nil
}

// evidenceChunks: potongan temuan beserta tetangga hasil aksi read, urut posisi di dokumen.
func evidenceChunks(ev *evidence) []chunkHit {
	out := []chunkHit{{ChunkID: ev.ChunkID, DocumentID: ev.Doc.ID, ChunkIndex: ev.ChunkIndex, Text: ev.Text}}
	out = append(out, ev.Neighbors...)
	sort.Slice(out, func(i, j int) bool { return out[i].ChunkIndex < out[j].ChunkIndex })
	return out
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// note menambahkan langkah keterangan (mis. alasan penelusuran dihentikan) yang langsung selesai.
func (a *agentRun) note(title, detail string) {
	st := a.tr.begin("think", title)
	st.Detail = detail
	a.tr.end(st)
}

var clauseSplitter = regexp.MustCompile(`(?i)\s*(?:[;?]|,|\bdan\b|\bserta\b|\blalu\b|\bkemudian\b)\s*`)

// builtinQueryPlan menyusun beberapa kueri tanpa LLM: pertanyaan asli, tiap bagian pertanyaan majemuk, kata kunci
// inti, varian sinonim, dan konteks pertanyaan sebelumnya bila ini pertanyaan lanjutan.
func builtinQueryPlan(question string, known map[string]bool, history []ChatTurn, limit int) []string {
	seen := map[string]bool{}
	var queries []string
	add := func(q string) {
		q = strings.TrimSpace(q)
		key := normalizeQuery(q)
		if key != "" && !seen[key] && len(queries) < limit {
			seen[key] = true
			queries = append(queries, q)
		}
	}
	add(question)
	terms := contentSearchTerms(question, known)
	for _, part := range clauseSplitter.Split(question, -1) {
		if len(contentSearchTerms(part, known)) > 0 {
			add(part)
		}
	}
	if len(terms) >= 2 {
		add(strings.Join(termDisplays(terms), " "))
	}
	if exp := synonymExpansions(question, terms); len(exp) > 0 {
		head := terms
		if len(head) > 3 {
			head = head[:3]
		}
		add(strings.Join(append(termDisplays(head), termDisplays(exp)...), " "))
	}
	if isFollowUpQuestion(question, len(terms), history) {
		for i := len(history) - 1; i >= 0; i-- {
			if len(contentSearchTerms(history[i].Prompt, known)) > 0 {
				add(history[i].Prompt + " " + question)
				break
			}
		}
	}
	return queries
}

func termDisplays(terms []searchTerm) []string {
	out := make([]string, len(terms))
	for i, t := range terms {
		out[i] = t.Display
	}
	return out
}

func (a *agentRun) runBuiltinAgent(history []ChatTurn) error {
	auto := a.cfg.ThinkingMaxSteps <= 0
	limit := builtinMaxQueries
	if !auto {
		limit = min(a.cfg.ThinkingMaxSteps, builtinMaxQueries)
	}
	queries := builtinQueryPlan(a.req.Prompt, a.known, history, limit)
	st := a.tr.begin("think", "Merencanakan pencarian")
	st.Queries = queries
	st.Detail = fmt.Sprintf("Mode tanpa LLM: pertanyaan diurai menjadi %d kueri — pertanyaan asli, bagian-bagiannya, kata kunci inti, dan sinonim istilah audit.", len(queries))
	a.tr.end(st)
	for _, q := range queries {
		if _, _, _, err := a.searchStep(q); err != nil {
			return err
		}
	}

	// Mode otomatis: perluas pencarian dari temuan teratas sampai tidak ada temuan baru
	for round := 1; auto && round <= builtinExpansionRounds && a.tr.trace.Searches < builtinSafetyMaxSearches; round++ {
		next := a.expansionQueries()
		if len(next) == 0 {
			break
		}
		st := a.tr.begin("think", "Memperluas pencarian dari temuan teratas")
		st.Queries = next
		st.Detail = "Judul dokumen yang paling relevan dipakai sebagai kueri baru untuk menemukan dokumen terkait (versi lain, lampiran, atau dokumen pendukung)."
		a.tr.end(st)
		fresh := 0
		for _, q := range next {
			if a.tr.trace.Searches >= builtinSafetyMaxSearches {
				break
			}
			_, n, _, err := a.searchStep(q)
			if err != nil {
				return err
			}
			fresh += n
		}
		if fresh == 0 {
			a.note("Penelusuran sudah jenuh", "Perluasan pencarian tidak menghasilkan temuan baru; menyimpulkan dari temuan yang ada.")
			break
		}
	}
	// Baca lebih dalam tiga temuan teratas (paling sering & paling tinggi muncul di berbagai kueri)
	top := append([]*evidence(nil), a.pool...)
	sort.SliceStable(top, func(i, j int) bool { return top[i].RRF > top[j].RRF })
	var refs []string
	for _, ev := range top {
		if len(refs) >= agentMaxReads {
			break
		}
		refs = append(refs, ev.Ref)
	}
	if len(refs) > 0 {
		if _, err := a.read(refs); err != nil {
			return err
		}
	}
	return nil
}

// expansionQueries: kueri baru dari judul dokumen temuan teratas yang belum pernah dicari, dilengkapi kata kunci inti
// pertanyaan (pseudo-relevance feedback) untuk menemukan dokumen terkait.
func (a *agentRun) expansionQueries() []string {
	top := append([]*evidence(nil), a.pool...)
	sort.SliceStable(top, func(i, j int) bool { return top[i].RRF > top[j].RRF })
	core := termDisplays(contentSearchTerms(a.req.Prompt, a.known))
	if len(core) > 2 {
		core = core[:2]
	}
	var out []string
	seenDoc := map[string]bool{}
	for _, ev := range top {
		if len(out) >= 3 {
			break
		}
		if seenDoc[ev.Doc.ID] {
			continue
		}
		seenDoc[ev.Doc.ID] = true
		q := ev.Doc.Title
		if app := ev.Doc.Application; app != nil && !textHasTerm(q, strings.ToLower(app.AppCode)) {
			q += " " + app.AppCode
		}
		for _, c := range core {
			if !textHasTerm(q, c) {
				q += " " + c
			}
		}
		if key := normalizeQuery(q); key != "" && !a.ran[key] {
			out = append(out, q)
		}
	}
	return out
}

// rerank menilai ulang seluruh temuan lalu memilih bukti yang beragam untuk jawaban.
func (a *agentRun) rerank(ctx context.Context, focus []searchTerm) []*evidence {
	st := a.tr.begin("rerank", "Menilai ulang temuan")
	before := append([]*evidence(nil), a.pool...)
	sort.SliceStable(before, func(i, j int) bool { return before[i].RRF > before[j].RRF })
	prevRank := map[string]int{}
	for i, ev := range before {
		prevRank[ev.Ref] = i + 1
	}

	rerankEvidence(a.pool, focus, latestSectionDocs(a.docs), a.metaSets)
	st.Rerank = a.modelRerankPool(ctx, st)
	selected := selectEvidence(a.pool)
	chosen := map[string]bool{}
	docs := map[string]bool{}
	for _, ev := range selected {
		chosen[ev.Ref] = true
		docs[ev.Doc.ID] = true
	}
	st.Results = a.stepResults(a.pool, 8, true)
	for i := range st.Results {
		st.Results[i].PrevRank = prevRank[st.Results[i].Ref]
		st.Results[i].Selected = chosen[st.Results[i].Ref]
	}
	agentNote := ""
	for _, ev := range a.pool {
		if ev.AgentRank > 0 {
			agentNote = ", dan pilihan agen"
			break
		}
	}
	st.Detail = fmt.Sprintf("%d temuan dari %d pencarian digabung (RRF) lalu dinilai ulang memakai skor kecocokan kata, cakupan & kedekatan kata kunci, metadata dokumen, versi terbaru%s. Terpilih %d bukti dari %d dokumen.",
		len(a.pool), a.tr.trace.Searches, agentNote, len(selected), len(docs))
	switch {
	case st.Rerank.Method == "model":
		st.Detail += fmt.Sprintf(" %d temuan teratas juga dinilai model %s:%s (%d ms; %s).", st.Rerank.Candidates, st.Rerank.Provider, st.Rerank.Model, st.Rerank.LatencyMs, st.Rerank.Weights)
	case st.Rerank.Fallback != "":
		st.Detail += " Reranker model tidak dipakai: " + st.Rerank.Fallback + "."
	}
	a.tr.trace.Findings = len(a.pool)
	a.tr.end(st)
	return selected
}

// modelRerankPool menilai ulang temuan teratas (hasil heuristik) dengan reranker model bila aktif, lalu mengurutkan
// ulang pool. Pertanyaan asli user menjadi kueri reranker.
func (a *agentRun) modelRerankPool(ctx context.Context, st *AgentStep) *RerankInfo {
	limit := a.rerankCfg.Candidates
	if limit <= 0 {
		limit = DefaultRerankCandidates
	}
	top := a.pool
	if len(top) > limit {
		top = top[:limit]
	}
	if st.Tool == nil {
		st.Tool = &ToolCallInfo{Name: "rerank"}
		defer func() { st.Tool = nil }()
	}
	ranked, scores, info := a.rerankCandidates(ctx, a.req.Prompt, top, len(top), st)
	if info.Method != "model" {
		return info
	}
	inTop := map[*evidence]bool{}
	for _, ev := range ranked {
		ev.Score = math.Round(scores[ev]*1000) / 1000
		inTop[ev] = true
	}
	rest := make([]*evidence, 0, len(a.pool))
	for _, ev := range a.pool {
		if !inTop[ev] {
			ev.Score *= 1 - rerankModelWeight // di luar kandidat reranker: tetap di bawah yang dinilai model
			rest = append(rest, ev)
		}
	}
	a.pool = append(ranked, rest...)
	sort.SliceStable(a.pool, func(i, j int) bool { return a.pool[i].Score > a.pool[j].Score })
	return info
}

// buildContext mengubah bukti terpilih menjadi kartu dokumen & potongan konteks per dokumen (urut posisi di dokumen).
func (a *agentRun) buildContext(selected []*evidence) ([]AIDocumentCitation, map[string][]chunkHit) {
	var order []string
	best := map[string]*evidence{}
	chunks := map[string]map[int]chunkHit{}
	for _, ev := range selected {
		id := ev.Doc.ID
		if best[id] == nil {
			best[id] = ev
			order = append(order, id)
			chunks[id] = map[int]chunkHit{}
		}
		for _, c := range evidenceChunks(ev) {
			chunks[id][c.ChunkIndex] = c
		}
	}
	citations := make([]AIDocumentCitation, 0, len(order))
	context := make(map[string][]chunkHit, len(order))
	for _, id := range order {
		citations = append(citations, buildCitation(*best[id].Doc, makeExcerpt(best[id].Text, a.keys, 280)))
		var list []chunkHit
		for _, c := range chunks[id] {
			list = append(list, c)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].ChunkIndex < list[j].ChunkIndex })
		if len(list) > 5 {
			list = list[:5]
		}
		context[id] = list
	}
	return citations, context
}

// missingTerms: kata dari pertanyaan yang tidak ada di temuan mana pun maupun metadata dokumen dalam cakupan.
func (a *agentRun) missingTerms(focus []searchTerm) []string {
	var out []string
	for _, t := range focus {
		if !t.primary() {
			continue
		}
		found := false
		for _, ev := range a.pool {
			if textHasTerm(ev.Text, t.Term) || textHasTerm(ev.Text, t.Display) {
				found = true
				break
			}
		}
		for id := range a.metaSets {
			if found {
				break
			}
			found = setHasTerm(a.metaSets[id], t.Term)
		}
		if !found {
			out = append(out, t.Display)
		}
	}
	return out
}

// runThinking menjawab dengan agen builtin: pencarian berulang, membaca bagian dokumen,
// rerank seluruh temuan, lalu menyusun jawaban dari bukti terpilih.
func runThinking(ctx context.Context, db *gorm.DB, user models.User, req AIChatRequest, cfg LLMConfig,
	retrievalHistory []ChatTurn, tr *tracer) (*AIChatResponse, error) {
	db = db.WithContext(ctx)
	a, err := newAgentRun(db, user, req, cfg, tr)
	if err != nil {
		return nil, err
	}
	a.rerankCfg = LoadRerankConfig(db)
	a.reranker = NewReranker(a.rerankCfg)
	focus := contentSearchTerms(req.Prompt, a.known)
	followUp := isFollowUpQuestion(req.Prompt, len(focus), retrievalHistory)
	if followUp {
		focus = append(focus, historyTerms(retrievalHistory, a.known, focus)...)
	}
	focus = append(focus, synonymExpansions(req.Prompt, focus)...)
	a.keys = excerptKeys(focus)

	if err := a.runBuiltinAgent(retrievalHistory); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	selected := a.rerank(ctx, focus)
	citations, contextChunks := a.buildContext(selected)
	missing := a.missingTerms(focus)
	scope := scopeLabelFor(user)
	resp := &AIChatResponse{ScopeLabel: scope, MatchedDocuments: citations}

	// Tanpa bukti: Document Guard + saran dokumen terdekat.
	if len(citations) == 0 {
		resp.ReplyMarkdown = buildNotFoundReply(scope, missing, len(a.docs)) + notFoundSuggestions(closestDocuments(a.docs, req.Prompt, 3))
		resp.EngineMode = "PANDA Document Guard (tidak ada dokumen relevan)"
		resp.AnswerBasis = AnswerBasisGeneral
		tr.delta(resp.ReplyMarkdown)
		return tr.finish(resp), nil
	}

	header := fmt.Sprintf("🧠 **Penelusuran mendalam:** %d pencarian, %d temuan dinilai ulang, %d dokumen dipakai sebagai bukti.\n\n",
		tr.trace.Searches, len(a.pool), len(citations))
	st := tr.begin("answer", fmt.Sprintf("Menyusun jawaban dari %d bukti", len(selected)))
	resp.ReplyMarkdown = header + buildBuiltinSmartSummary(scope, citations, contextChunks, len(a.docs), false, missing)
	resp.EngineMode = "PANDA Thinking (Built-in)"
	resp.AnswerBasis = AnswerBasisDocuments
	tr.delta(resp.ReplyMarkdown)
	tr.end(st)
	return tr.finish(resp), nil
}
