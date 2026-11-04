package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"
	"panda-backend/internal/models"
)

func TestHTTPIdentityAndImpersonationContract(t *testing.T) {
	for _, tc := range []struct {
		name, role, method, path string
		impersonate, inactive    bool
		want                     int
	}{
		{"authenticated", models.RolePegawaiTI, "GET", "/auth/me", false, false, 200},
		{"inactive", models.RolePegawaiTI, "GET", "/auth/me", false, true, 401},
		{"employee cannot impersonate", models.RolePegawaiTI, "GET", "/auth/me", true, false, 403},
		{"admin cannot impersonate", models.RoleAdmin, "GET", "/auth/me", true, false, 403},
		{"super admin read", models.RoleSuperAdmin, "GET", "/auth/me", true, false, 200},
		{"super admin mutation blocked", models.RoleSuperAdmin, "POST", "/folders", true, false, 403},
		{"super admin chat allowed", models.RoleSuperAdmin, "POST", "/ai/chat", true, false, 200},
		{"super admin stream allowed", models.RoleSuperAdmin, "POST", "/ai/chat/stream", true, false, 200},
		{"chat lookalike blocked", models.RoleSuperAdmin, "POST", "/ai/chat/other", true, false, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := authorizationDB(t)
			if err := db.Callback().Query().Remove("gorm:preload"); err != nil {
				t.Fatal(err)
			}
			if err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
				switch dest := tx.Statement.Dest.(type) {
				case *models.User:
					if tc.inactive {
						tx.AddError(gorm.ErrRecordNotFound)
						return
					}
					*dest = models.User{ID: "actor", BadgeNumber: "badge", Role: tc.role, IsActive: true}
				case *models.MasterTeam:
					*dest = models.MasterTeam{ID: "effective-team"}
				default:
					t.Errorf("unexpected query %T", dest)
				}
			}); err != nil {
				t.Fatal(err)
			}
			h := &Handler{DB: db, JWTSecret: []byte("test-secret-that-is-at-least-32-bytes")}
			r := fiber.New(HTTPConfig())
			r.Add([]string{tc.method}, "/api/v1"+tc.path, h.AuthMiddleware(), h.GetMe)
			token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"badge_number": "badge", "exp": time.Now().Add(time.Hour).Unix()}).SignedString(h.JWTSecret)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(tc.method, "/api/v1"+tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			if tc.impersonate {
				req.Header.Set(headerImpersonateTeam, "effective-team")
			}
			w := testHTTP(t, r, req)
			if w.Code != tc.want {
				t.Fatalf("got %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want != http.StatusOK {
				return
			}
			var response struct {
				User          models.User `json:"user"`
				Real          models.User `json:"real_user"`
				Impersonating bool        `json:"impersonating"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Real.ID != "actor" || response.Real.Role != tc.role || response.Impersonating != tc.impersonate {
				t.Fatalf("actor lost: %+v", response)
			}
			if tc.impersonate && (response.User.Role != models.RolePegawaiTI || response.User.TeamID == nil || *response.User.TeamID != "effective-team") {
				t.Fatalf("effective scope lost: %+v", response)
			}
		})
	}
}
