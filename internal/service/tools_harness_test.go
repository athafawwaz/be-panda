package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"panda-backend/internal/models"
)

func TestToolsHarnessStrictArguments(t *testing.T) {
	for _, tool := range agentToolset {
		if tool.Def.Function.Parameters["additionalProperties"] != false {
			t.Fatalf("%s permits undeclared arguments", tool.Def.Function.Name)
		}
	}
	cases := []struct {
		name  string
		tool  string
		args  map[string]any
		valid bool
	}{
		{"missing", "search_documents", nil, false},
		{"empty", "search_documents", map[string]any{"query": "  "}, false},
		{"null", "search_documents", map[string]any{"query": nil}, false},
		{"number string", "search_documents", map[string]any{"query": float64(3)}, false},
		{"array string", "read_document", map[string]any{"ref": []any{"D1"}}, false},
		{"object string", "get_application", map[string]any{"app": map[string]any{}}, false},
		{"alias", "get_application", map[string]any{"app": "vista", "app_code": "vista"}, false},
		{"unknown", "list_documents", map[string]any{"team": "secret"}, false},
		{"integer string", "list_documents", map[string]any{"limit": "2"}, false},
		{"fraction", "list_documents", map[string]any{"limit": 2.5}, false},
		{"zero", "list_documents", map[string]any{"limit": float64(0)}, false},
		{"over max", "list_documents", map[string]any{"limit": 21}, false},
		{"bool", "list_documents", map[string]any{"limit": true}, false},
		{"nan", "list_documents", map[string]any{"limit": math.NaN()}, false},
		{"infinity", "list_documents", map[string]any{"limit": math.Inf(1)}, false},
		{"bad enum", "list_documents", map[string]any{"category": "PRIVATE"}, false},
		{"empty enum", "list_documents", map[string]any{"sort": ""}, false},
		{"category case", "list_documents", map[string]any{"category": " uat ", "module": "application", "sort": "TITLE", "limit": 20}, true},
		{"group case", "get_repository_stats", map[string]any{"group_by": "APPLICATION"}, true},
		{"json integer", "search_documents", map[string]any{"query": "UAT", "top_k": float64(1)}, true},
		{"go integer", "list_documents", map[string]any{"limit": 1}, true},
		{"optional", "list_documents", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateToolArgs(findTool(tc.tool), tc.args)
			if (err == "") != tc.valid {
				t.Fatalf("valid=%v: %s", tc.valid, err)
			}
		})
	}
	if argString(map[string]any{"x": 2.0}, "x") != "" || argInt(map[string]any{"x": "2"}, "x", 5, 1, 20) != 5 || argInt(map[string]any{"x": 2}, "x", 5, 1, 20) != 2 {
		t.Fatal("internal helpers must not coerce strings or ignore Go integers")
	}
}

func toolsHarnessPayload(t *testing.T, res toolResult) map[string]any {
	t.Helper()
	if len(res.Output) > toolResultMaxChars {
		t.Fatalf("tool result exceeds byte bound: %d", len(res.Output))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(res.Output), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["truncated"] != res.Truncated {
		t.Fatalf("truncation differs: payload=%v result=%v", payload, res.Truncated)
	}
	if _, ok := payload["returned"]; !ok {
		t.Fatalf("missing returned count: %v", payload)
	}
	return payload
}

func TestToolsHarnessListTotals(t *testing.T) {
	a := newTestToolAgent(LLMConfig{}, "x", false, nil)
	for _, tc := range []struct {
		args  map[string]any
		total float64
	}{
		{map[string]any{"title_contains": "backup"}, 1},
		{map[string]any{"title_contains": "pencadangan"}, 1},
		{map[string]any{"uploaded_after": "2026-10-15"}, 1},
		{map[string]any{"title_contains": "backup", "uploaded_after": "2026-10-15"}, 0},
		{map[string]any{"limit": 1}, 2},
	} {
		res := runListDocumentsTool(context.Background(), a, tc.args, nil)
		p := toolsHarnessPayload(t, res)
		if res.Err != "" || p["total_matching"] != tc.total || p["total_in_scope_matching_filters"] != tc.total || p["returned"] != float64(res.Count) {
			t.Fatalf("args=%v result=%+v payload=%v", tc.args, res, p)
		}
		if tc.total == 2 && (!res.Truncated || res.Count != 1) {
			t.Fatalf("limit must remain visible: %+v", res)
		}
	}
}

func TestToolsHarnessFitJSONBound(t *testing.T) {
	for _, n := range []int{0, 3} {
		out, shown, cut, msg := fitJSON(func(int) any {
			return map[string]any{"metadata": strings.Repeat("界", toolResultMaxChars)}
		}, n)
		if len(out) > toolResultMaxChars || !json.Valid([]byte(out)) || shown != 0 || !cut || msg == "" {
			t.Fatalf("hard bound failed: bytes=%d shown=%d cut=%v error=%q", len(out), shown, cut, msg)
		}
		p := toolsHarnessPayload(t, errorResult(msg))
		if p["error"] == nil {
			t.Fatal("oversize metadata needs explicit error")
		}
	}
	out, shown, cut, msg := fitJSON(func(n int) any {
		return map[string]any{"items": strings.Repeat("界", n*800), "total_matching": 3}
	}, 3)
	if msg != "" || !cut || shown >= 3 || len(out) > toolResultMaxChars || !strings.Contains(out, `"truncated":true`) {
		t.Fatalf("JSON cut must be visible: shown=%d cut=%v msg=%s output=%s", shown, cut, msg, out)
	}
	a := newTestToolAgent(LLMConfig{}, "x", false, nil)
	a.docs[0].Title = strings.Repeat("界", toolResultMaxChars)
	res := runListDocumentsTool(context.Background(), a, map[string]any{"category": "UAT"}, nil)
	p := toolsHarnessPayload(t, res)
	if res.Count != 0 || !res.Truncated || p["total_matching"] != float64(1) {
		t.Fatalf("item dropped by JSON budget must be visible: %+v", res)
	}
}

func TestToolsHarnessSearchKeepsUnmatchedApplication(t *testing.T) {
	a := newTestToolAgent(LLMConfig{}, "x", false, nil)
	st := &AgentStep{Tool: &ToolCallInfo{}}
	res := runSearchDocumentsTool(context.Background(), a, map[string]any{"query": "backup", "app_code": "Digimon"}, st)
	p := toolsHarnessPayload(t, res)
	if res.Err != "" || res.Count != 0 || p["query"] != "backup" || p["filters"].(map[string]any)["app_code"] != "Digimon" || p["filter_note"] != nil {
		t.Fatalf("unmatched app filter must not broaden search: %s", res.Output)
	}
}

func toolsHarnessDB(t *testing.T, dryRun bool) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.Open("host=localhost user=unused password=unused dbname=unused sslmode=disable"), &gorm.Config{
		DryRun: dryRun, DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func TestToolsHarnessScopedApplicationsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := newTestToolAgent(LLMConfig{}, "x", false, nil)
	a.db = toolsHarnessDB(t, false).WithContext(ctx)
	if _, err := a.scopedApplications("vista", 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("must preserve DB context error: %v", err)
	}
	res := runGetApplicationTool(ctx, a, map[string]any{"app": "vista"}, nil)
	p := toolsHarnessPayload(t, res)
	if res.Err == "" || p["error"] == nil || strings.Contains(res.Output, "unused") || strings.Contains(res.Output, "context canceled") {
		t.Fatalf("DB failure must be a safe tool error: %+v", res)
	}
}

func TestToolsHarnessScopedApplicationCodeCase(t *testing.T) {
	a := newTestToolAgent(LLMConfig{}, "x", false, nil)
	a.db = toolsHarnessDB(t, true)
	var sql string
	if err := a.db.Callback().Query().After("gorm:query").Register("harness:sql", func(tx *gorm.DB) { sql = tx.Statement.SQL.String() }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.scopedApplications("vista", 5); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "LOWER(app_code) = LOWER(") || !strings.Contains(sql, "folder_id IN") {
		t.Fatalf("case-insensitive lookup must retain folder scope: %s", sql)
	}
}

func TestToolsHarnessReadClippedSections(t *testing.T) {
	for _, text := range []string{strings.Repeat("a", 2000), strings.Repeat("界", 2000)} {
		a := newTestToolAgent(LLMConfig{ShareDocContent: true}, "x", false, nil)
		a.db = toolsHarnessDB(t, true)
		if err := a.db.Callback().Query().After("gorm:query").Register("harness:neighbors", func(tx *gorm.DB) {
			if rows, ok := tx.Statement.Dest.(*[]models.DocumentChunk); ok {
				*rows = []models.DocumentChunk{{ID: "neighbor", ChunkIndex: 1, ChunkText: text}}
			}
		}); err != nil {
			t.Fatal(err)
		}
		doc := &a.docs[0]
		a.byRef["E1"] = &evidence{Ref: "E1", ChunkID: "first", ChunkIndex: 0, Doc: doc, Text: "first section"}
		res := runReadDocumentTool(context.Background(), a, map[string]any{"ref": "E1"}, &AgentStep{Tool: &ToolCallInfo{}})
		p := toolsHarnessPayload(t, res)
		if res.Err != "" || !res.Truncated || res.Count != 1 || p["returned"] != float64(1) || !strings.Contains(res.Output, `"ref":"E1"`) {
			t.Fatalf("clipped content must be visible and retain refs: %+v", res)
		}
	}
	a := newTestToolAgent(LLMConfig{}, "x", false, nil)
	refs := make([]string, agentMaxReads+1)
	for i := range refs {
		refs[i] = fmt.Sprintf("E%d", i+1)
	}
	res := runReadDocumentTool(context.Background(), a, map[string]any{"ref": strings.Join(refs, ",")}, nil)
	toolsHarnessPayload(t, res)
	if res.Err == "" {
		t.Fatal("too many read refs must not be silently sliced")
	}
}

func TestToolsHarnessApplicationAndStatsCounts(t *testing.T) {
	a := newTestToolAgent(LLMConfig{}, "x", false, nil)
	for _, key := range []string{"vista", "not-an-app"} {
		res := runGetApplicationTool(context.Background(), a, map[string]any{"app": key}, nil)
		toolsHarnessPayload(t, res)
		if res.Err != "" {
			t.Fatal(res.Err)
		}
	}
	res := runRepositoryStatsTool(context.Background(), a, nil, nil)
	p := toolsHarnessPayload(t, res)
	if p["total_matching"] != float64(2) || p["returned"] != float64(2) {
		t.Fatalf("bad stats counts: %s", res.Output)
	}
	// Many versions force JSON cuts; step results must describe only serialized refs.
	app := a.docs[0].Application
	app.Sections = nil
	a.docs = nil
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("section-%d", i)
		app.Sections = append(app.Sections, models.AppChangeSection{ID: id, ApplicationID: app.ID, VersionLabel: fmt.Sprint(i), SectionTitle: strings.Repeat("v", 80)})
		a.docs = append(a.docs, models.Document{ID: fmt.Sprintf("doc-%d", i), Application: app, SectionID: &id, Title: "Doc", UploadedAt: time.Now()})
	}
	res = runGetApplicationTool(context.Background(), a, map[string]any{"app": "vista"}, nil)
	toolsHarnessPayload(t, res)
	if !res.Truncated || res.Count >= 30 {
		t.Fatalf("expected limited versions: %+v", res)
	}
	for _, result := range res.Results {
		if !strings.Contains(res.Output, `"ref":"`+result.Ref+`"`) {
			t.Fatalf("hidden ref in results: %s", result.Ref)
		}
	}
}
