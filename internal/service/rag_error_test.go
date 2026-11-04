package service

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"panda-backend/internal/models"
)

var errRetrievalDB = errors.New("retrieval database unavailable")

type retrievalConnector struct {
	failAt int
	calls  int
	like   bool
}

func (c *retrievalConnector) Connect(context.Context) (driver.Conn, error) { return c, nil }
func (c *retrievalConnector) Driver() driver.Driver                        { return c }
func (c *retrievalConnector) Open(string) (driver.Conn, error)             { return c, nil }
func (c *retrievalConnector) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c *retrievalConnector) Close() error { return nil }
func (c *retrievalConnector) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected transaction")
}
func (c *retrievalConnector) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.calls++
	if c.calls == c.failAt {
		return nil, errRetrievalDB
	}
	switch {
	case strings.Contains(query, "document_chunk_terms AS t"):
		return &retrievalRows{columns: []string{"chunk_id", "document_id", "term", "tf", "token_count"}, values: []driver.Value{"c1", "d1", "uat", int64(1), int64(5)}}, nil
	case strings.Contains(query, "AVG("):
		return &retrievalRows{columns: []string{"avg"}, values: []driver.Value{float64(5)}}, nil
	case strings.Contains(query, "count(*)"):
		return &retrievalRows{columns: []string{"count"}, values: []driver.Value{int64(0)}}, nil
	case c.like:
		return &retrievalRows{columns: []string{"id", "document_id", "chunk_index", "chunk_text"}, values: []driver.Value{"c1", "d1", int64(0), "uat lulus"}}, nil
	default:
		return &retrievalRows{columns: []string{"id"}}, nil
	}
}

type retrievalRows struct {
	columns []string
	values  []driver.Value
}

func (r *retrievalRows) Columns() []string { return r.columns }
func (r *retrievalRows) Close() error      { return nil }
func (r *retrievalRows) Next(dest []driver.Value) error {
	if r.values == nil {
		return io.EOF
	}
	copy(dest, r.values)
	r.values = nil
	return nil
}

func retrievalTestDB(t *testing.T, failAt int, like bool) *gorm.DB {
	t.Helper()
	conn := sql.OpenDB(&retrievalConnector{failAt: failAt, like: like})
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestRetrievalDatabaseFailures(t *testing.T) {
	terms := []searchTerm{{Term: "uat", Display: "uat", Weight: 1}}
	for _, tc := range []struct {
		name   string
		failAt int
		like   bool
	}{
		{"stale count", 1, false}, {"index find", 2, false}, {"index count", 3, false}, {"index average", 4, false},
		{"text find", 1, true}, {"text count", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := retrievalTestDB(t, tc.failAt, tc.like)
			scope := func() *gorm.DB { return db.Model(&models.Document{}).Select("id") }
			var err error
			if tc.like {
				_, _, err = retrieveChunksByLike(db, scope, terms)
			} else {
				_, _, err = retrieveRelevantChunks(db, scope, terms)
			}
			if !errors.Is(err, errRetrievalDB) {
				t.Fatalf("expected DB failure, got %v", err)
			}
		})
	}
	t.Run("load texts", func(t *testing.T) {
		err := loadChunkTexts(retrievalTestDB(t, 1, false), map[string][]chunkHit{"d1": {{ChunkID: "c1"}}})
		if !errors.Is(err, errRetrievalDB) {
			t.Fatalf("expected DB failure, got %v", err)
		}
	})
	t.Run("leading chunks", func(t *testing.T) {
		_, err := loadLeadingChunks(retrievalTestDB(t, 1, false), []string{"d1"})
		if !errors.Is(err, errRetrievalDB) {
			t.Fatalf("expected DB failure, got %v", err)
		}
	})
	t.Run("neighbors do not mark failed reads", func(t *testing.T) {
		a := newAgentRunWithDocs(retrievalTestDB(t, 1, false), models.User{}, AIChatRequest{}, LLMConfig{}, newTracer(false, nil), nil)
		ev := &evidence{Doc: &models.Document{ID: "d1"}, ChunkID: "c1"}
		first, err := a.loadNeighbors(ev)
		if first || !errors.Is(err, errRetrievalDB) || a.readChunks["c1"] || a.readDocs["d1"] {
			t.Fatalf("failed read recorded as success: first=%v err=%v", first, err)
		}
	})
	t.Run("builtin search", func(t *testing.T) {
		a := newAgentRunWithDocs(retrievalTestDB(t, 1, false), models.User{}, AIChatRequest{Prompt: "uat"}, LLMConfig{}, newTracer(false, nil), nil)
		if err := a.runBuiltinAgent(nil); !errors.Is(err, errRetrievalDB) {
			t.Fatalf("expected DB failure, got %v", err)
		}
	})
	t.Run("retrieve context", func(t *testing.T) {
		_, err := RetrieveContext(retrievalTestDB(t, 1, false), models.User{}, AIChatRequest{Prompt: "uat"}, nil, LLMConfig{})
		if !errors.Is(err, errRetrievalDB) {
			t.Fatalf("expected DB failure, got %v", err)
		}
	})
	t.Run("history ownership", func(t *testing.T) {
		_, err := LoadRecentTurns(retrievalTestDB(t, 1, false), "session", "owner", 3)
		if !errors.Is(err, errRetrievalDB) {
			t.Fatalf("expected DB failure, got %v", err)
		}
	})
	t.Run("history messages", func(t *testing.T) {
		// Count zero is a normal unowned session, not a database error.
		turns, err := LoadRecentTurns(retrievalTestDB(t, 0, false), "session", "owner", 3)
		if err != nil || len(turns) != 0 {
			t.Fatalf("unowned session: turns=%v err=%v", turns, err)
		}
	})
	t.Run("reply reference", func(t *testing.T) {
		_, _, err := resolveReplyRef(retrievalTestDB(t, 1, false), "owner", &ChatReplyRef{MessageID: 1, Text: "untrusted fallback"}, false)
		if !errors.Is(err, errRetrievalDB) {
			t.Fatalf("DB failure must not use client fallback: %v", err)
		}
	})
}

func TestRetrievalCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := retrievalTestDB(t, 0, false).WithContext(ctx)
	_, err := RetrieveContext(db, models.User{}, AIChatRequest{Prompt: "uat"}, nil, LLMConfig{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled retrieval, got %v", err)
	}
	resp, err := AnswerAgenticQuery(ctx, nil, models.User{}, AIChatRequest{}, "", nil)
	if resp != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled answer before DB/config access: resp=%v err=%v", resp, err)
	}
}

func TestFilteredScopeAppCodeIsCaseInsensitive(t *testing.T) {
	db := retrievalTestDB(t, 0, false)
	a := newAgentRunWithDocs(db, models.User{Role: models.RoleSuperAdmin}, AIChatRequest{}, LLMConfig{}, newTracer(false, nil), nil)
	query := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return tx.Where("id IN (?)", a.filteredScope(docFilter{AppCode: "corepay"})()).Find(&[]models.Document{})
	})
	if !strings.Contains(query, "LOWER(app_code) = LOWER('corepay')") {
		t.Fatalf("app filter must match appMatches EqualFold: %s", query)
	}
}
