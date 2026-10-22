package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"panda-backend/internal/models"
)

var ErrApplicationCodeUsed = errors.New("kode aplikasi sudah digunakan")

// CreateApplication menyimpan aplikasi, section awal, dan audit sebagai satu unit kerja.
// Handler menyiapkan data tervalidasi; hasil hanya dikembalikan setelah transaksi berhasil.
func CreateApplication(ctx context.Context, db *gorm.DB, app models.Application, initial models.AppChangeSection, audit models.UploadAuditLog) (*models.Application, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&app).Error; err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "applications_app_code_key" {
				return ErrApplicationCodeUsed
			}
			return fmt.Errorf("create application: %w", err)
		}
		initial.ApplicationID = app.ID
		if err := tx.Create(&initial).Error; err != nil {
			return fmt.Errorf("create initial application section: %w", err)
		}
		if err := tx.Create(&audit).Error; err != nil {
			return fmt.Errorf("record application audit: %w", err)
		}
		if err := tx.Preload("Sections").Preload("Sections.Documents").First(&app, "id = ?", app.ID).Error; err != nil {
			return fmt.Errorf("load created application: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &app, nil
}
