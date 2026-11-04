package handler

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v3"
)

type requestValidator struct{ validator *validator.Validate }

func (v requestValidator) Validate(out any) error { return v.validator.Struct(out) }

func HTTPConfig() fiber.Config {
	validate := validator.New()
	validate.SetTagName("binding")
	return fiber.Config{
		Immutable:     true,
		CaseSensitive: true,
		StrictRouting: true,
		ReadTimeout:   30 * time.Second,
		IdleTimeout:   60 * time.Second,
		// Allow bounded Thinking + final generation; slow writes still have a ceiling.
		WriteTimeout:                 20 * time.Minute,
		BodyLimit:                    int(maxUploadBytes() + (1 << 20)),
		DisablePreParseMultipartForm: true,
		StructValidator:              requestValidator{validate},
		ErrorHandler:                 HTTPError,
	}
}

func HTTPError(c fiber.Ctx, err error) error {
	status, message := http.StatusInternalServerError, "Terjadi kesalahan internal"
	var httpError *fiber.Error
	if errors.As(err, &httpError) {
		status, message = httpError.Code, httpError.Message
	} else {
		log.Printf("[HTTP] %v", err)
	}
	if status == http.StatusRequestEntityTooLarge {
		message = fmt.Sprintf("Ukuran file melebihi batas %d MB", maxUploadBytes()>>20)
	}
	return c.Status(status).JSON(fiber.Map{"error": message})
}
