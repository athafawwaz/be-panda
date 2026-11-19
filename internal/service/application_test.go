package service

import (
	"context"
	"errors"
	"testing"

	"panda-backend/internal/models"
)

func TestCreateApplicationCanceledBeforeDatabaseAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app, err := CreateApplication(ctx, nil, models.Application{}, models.AppChangeSection{}, models.UploadAuditLog{})
	if app != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled creation must not access the database: app=%+v err=%v", app, err)
	}
}
