package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"panda-backend/internal/models"
	"panda-backend/internal/service"
)

func TestMiddlewareRejectsInvalidCredentials(t *testing.T) {
	h := &Handler{JWTSecret: []byte("test-secret-that-is-at-least-32-bytes")}
	for _, tc := range []struct {
		name   string
		method jwt.SigningMethod
		claims jwt.MapClaims
		query  bool
	}{
		{name: "missing bearer"},
		{name: "query token is not accepted", query: true},
		{name: "missing expiration", method: jwt.SigningMethodHS256, claims: jwt.MapClaims{"badge_number": "test"}},
		{name: "expired", method: jwt.SigningMethodHS256, claims: jwt.MapClaims{"exp": time.Now().Add(-time.Hour).Unix()}},
		{name: "wrong algorithm", method: jwt.SigningMethodHS512, claims: jwt.MapClaims{"exp": time.Now().Add(time.Hour).Unix()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.GET("/protected", h.AuthMiddleware(), func(c *gin.Context) { t.Error("unauthorized request reached handler") })
			req := httptest.NewRequest(http.MethodGet, "/protected", nil)
			if tc.query {
				req.URL.RawQuery = "token=untrusted"
			} else if tc.method != nil {
				token, err := jwt.NewWithClaims(tc.method, tc.claims).SignedString(h.JWTSecret)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("got %d, want 401", w.Code)
			}
		})
	}
}

func authorizationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.Open("host=localhost user=unused password=unused dbname=unused sslmode=disable"), &gorm.Config{
		DryRun: true, DisableAutomaticPing: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestApplicationHandlersRejectOtherSegments(t *testing.T) {
	team, branch, otherBranch := "team-a", "branch-a", "branch-b"
	for _, folder := range []models.AppFolder{
		{AssignedTeamID: "team-b"},
		{AssignedTeamID: team, AssignedBranchID: &otherBranch},
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(method+folder.AssignedTeamID+"/"+folder.ID, func(t *testing.T) {
				db := authorizationDB(t)
				if err := db.Callback().Query().Remove("gorm:preload"); err != nil {
					t.Fatal(err)
				}
				if err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
					app, ok := tx.Statement.Dest.(*models.Application)
					if !ok {
						t.Fatalf("unexpected query destination %T", tx.Statement.Dest)
					}
					*app = models.Application{ID: "foreign-app", Folder: &folder}
				}); err != nil {
					t.Fatal(err)
				}
				h := &Handler{DB: db}
				r := gin.New()
				r.Use(func(c *gin.Context) {
					c.Set("currentUser", models.User{Role: models.RolePegawaiTI, TeamID: &team, BranchID: &branch})
				})
				r.GET("/applications/:id", h.GetApplicationDetail)
				r.POST("/applications/:id", h.CreateAppSection)
				req := httptest.NewRequest(method, "/applications/foreign-app", strings.NewReader(`{"version_label":"v2","section_title":"Forbidden"}`))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				if w.Code != http.StatusNotFound {
					t.Fatalf("got %d, want 404; %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestDocumentAndAuditScopes(t *testing.T) {
	db := authorizationDB(t)
	team, branch := "team-a", "branch-a"
	for _, tc := range []struct {
		name string
		user models.User
		want []string
	}{
		{"no team", models.User{Role: models.RolePegawaiTI}, []string{"visibility_scope = 'ALL_TIM'"}},
		{"team", models.User{Role: models.RolePegawaiTI, TeamID: &team}, []string{"owner_team_id = 'team-a'"}},
		{"branch", models.User{Role: models.RolePegawaiTI, TeamID: &team, BranchID: &branch}, []string{"owner_team_id = 'team-a'", "owner_branch_id = 'branch-a'"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sql := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
				return service.ApplyDocumentScope(tx.Model(&models.Document{}), tc.user).Where("id = ?", "foreign-doc").Find(&models.Document{})
			})
			for _, want := range append(tc.want, "id = 'foreign-doc'") {
				if !strings.Contains(sql, want) {
					t.Errorf("SQL missing %q: %s", want, sql)
				}
			}
			for _, impersonating := range []bool{false, true} {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Set("currentUser", tc.user)
				if impersonating {
					c.Set("realUser", models.User{Role: models.RoleSuperAdmin})
				}
				h := &Handler{DB: db}
				sql = db.ToSQL(func(tx *gorm.DB) *gorm.DB {
					h.DB = tx
					return h.scopedUploadLogs(c).Find(&[]models.UploadAuditLog{})
				})
				for _, want := range append(tc.want, "document_id IN (", "action_type <> 'AI_SEARCH'") {
					if !strings.Contains(sql, want) {
						t.Errorf("audit SQL missing %q: %s", want, sql)
					}
				}
				if strings.Contains(sql, "LIKE") || (impersonating && strings.Contains(sql, "badge_number =")) {
					t.Errorf("audit scope must not use labels or impersonated actor badge: %s", sql)
				}
			}
		})
	}
	for _, role := range []string{models.RoleAdmin, models.RoleSuperAdmin} {
		sql := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
			return service.ApplyDocumentScope(tx.Model(&models.Document{}), models.User{Role: role}).Find(&[]models.Document{})
		})
		if strings.Contains(sql, "WHERE") {
			t.Errorf("admin document access must stay unrestricted: %s", sql)
		}
	}
}
