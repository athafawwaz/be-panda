package service

import (
	"slices"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"panda-backend/internal/models"
)

func TestApplyDocumentScope(t *testing.T) {
	db, err := gorm.Open(postgres.Open("host=localhost user=unused dbname=unused sslmode=disable"), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	team, branch := "team'bound", "branch'bound"
	for _, tc := range []struct {
		name      string
		user      models.User
		predicate string
		vars      []any
	}{
		{"admin", models.User{Role: models.RoleAdmin}, "", nil},
		{"super admin", models.User{Role: models.RoleSuperAdmin}, "", nil},
		{"no team", models.User{Role: models.RolePegawaiTI}, "visibility_scope = 'ALL_TIM'", nil},
		{"branch without team", models.User{Role: models.RolePegawaiTI, BranchID: &branch}, "visibility_scope = 'ALL_TIM'", nil},
		{"team", models.User{Role: models.RolePegawaiTI, TeamID: &team}, "(visibility_scope = 'ALL_TIM') OR (owner_team_id = $1)", []any{team}},
		{"branch", models.User{Role: models.RolePegawaiTI, TeamID: &team, BranchID: &branch}, "(visibility_scope = 'ALL_TIM') OR (owner_team_id = $1 AND (owner_branch_id IS NULL OR owner_branch_id = '' OR owner_branch_id = $2))", []any{team, branch}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := ApplyDocumentScope(db.Model(&models.Document{}), tc.user).Find(&[]models.Document{})
			want := `SELECT * FROM "documents"`
			if tc.predicate != "" {
				want += " WHERE " + tc.predicate
			}
			if query.Statement.SQL.String() != want || !slices.Equal(query.Statement.Vars, tc.vars) {
				t.Fatalf("scope SQL=%s vars=%v; want SQL=%s vars=%v", query.Statement.SQL.String(), query.Statement.Vars, want, tc.vars)
			}
			if strings.Contains(query.Statement.SQL.String(), team) || strings.Contains(query.Statement.SQL.String(), branch) {
				t.Fatal("identity values must remain bound parameters")
			}
		})
	}
}
