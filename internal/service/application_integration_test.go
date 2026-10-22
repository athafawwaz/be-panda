//go:build integration

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"panda-backend/internal/models"
)

func TestIntegrationCreateApplication(t *testing.T) {
	db := openIntegrationDB(t)
	for _, failAt := range []string{"", "applications", "app_change_sections", "upload_audit_logs", "reload"} {
		name := failAt
		if name == "" {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			// A rollback outside the use case also isolates data when PANDA_IT_DB_NAME is used.
			tx := db.Begin()
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			defer tx.Rollback()
			folder := models.AppFolder{ID: uuid.NewString(), FolderName: "Application transaction", AssignedTeamID: "team-digitalisasi"}
			if err := tx.Create(&folder).Error; err != nil {
				t.Fatal(err)
			}
			app := models.Application{ID: uuid.NewString(), FolderID: folder.ID, AppCode: uuid.NewString(), AppName: "Atomic application"}
			section := models.AppChangeSection{ID: uuid.NewString(), VersionLabel: "v1.0", SectionTitle: "Initial", IsInitialRelease: true, SortOrder: 1}
			audit := models.UploadAuditLog{ActionType: "CREATE_APPLICATION", ModuleSource: "APPLICATION", AppName: app.AppName, ActionTimestamp: time.Now()}
			injected := errors.New("injected application write failure")
			callback := "test:create_application_failure"
			injecting := true
			if failAt != "" {
				callbacks := tx.Callback().Create()
				if failAt == "reload" {
					if err := tx.Callback().Query().Before("gorm:query").Register(callback, func(q *gorm.DB) {
						if injecting {
							q.AddError(injected)
						}
					}); err != nil {
						t.Fatal(err)
					}
					defer tx.Callback().Query().Remove(callback)
				} else {
					if err := callbacks.Before("gorm:create").Register(callback, func(q *gorm.DB) {
						if injecting && q.Statement.Table == failAt {
							q.AddError(injected)
						}
					}); err != nil {
						t.Fatal(err)
					}
					defer callbacks.Remove(callback)
				}
			}
			created, err := CreateApplication(context.Background(), tx, app, section, audit)
			injecting = false
			if failAt != "" {
				if !errors.Is(err, injected) || created != nil {
					t.Fatalf("failure accepted: app=%+v err=%v", created, err)
				}
			} else if err != nil || created == nil || len(created.Sections) != 1 || created.Sections[0].ApplicationID != created.ID {
				t.Fatalf("application must include its persisted initial section: app=%+v err=%v", created, err)
			}
			want := int64(0)
			if failAt == "" {
				want = 1
			}
			for _, model := range []any{&models.Application{}, &models.AppChangeSection{}, &models.UploadAuditLog{}} {
				var count int64
				q := tx.Model(model)
				switch model.(type) {
				case *models.Application:
					q = q.Where("id = ?", app.ID)
				case *models.AppChangeSection:
					q = q.Where("id = ?", section.ID)
				case *models.UploadAuditLog:
					q = q.Where("app_name = ?", app.AppName)
				}
				if err := q.Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if count != want {
					t.Errorf("%T count=%d want=%d after %s", model, count, want, failAt)
				}
			}
			if failAt == "" {
				duplicate := app
				duplicate.ID = uuid.NewString()
				if got, err := CreateApplication(context.Background(), tx, duplicate, section, audit); got != nil || !errors.Is(err, ErrApplicationCodeUsed) {
					t.Fatalf("duplicate code must remain a user error: app=%+v err=%v", got, err)
				}
			}
		})
	}
}
