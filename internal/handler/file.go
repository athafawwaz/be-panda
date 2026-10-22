package handler

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"

	"time"

	"github.com/gofiber/fiber/v3"
	"panda-backend/internal/storage"
)

func sendDocument(c fiber.Ctx, name, disposition string, file storage.ReadSeekCloser, info storage.Info) error {
	c.Set("Cache-Control", "private, no-store")
	c.Set("X-Content-Type-Options", "nosniff")
	c.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	modified := info.ModTime.UTC().Truncate(time.Second)
	if !modified.IsZero() {
		c.Set("Last-Modified", modified.Format(http.TimeFormat))
	}
	closeFile := func() {
		if err := file.Close(); err != nil {
			log.Printf("[Storage] close: %v", err)
		}
	}
	if since, err := http.ParseTime(c.Get("If-Unmodified-Since")); err == nil && modified.After(since) {
		closeFile()
		return c.SendStatus(http.StatusPreconditionFailed)
	}
	if since, err := http.ParseTime(c.Get("If-Modified-Since")); err == nil && !modified.After(since) && (c.Method() == "GET" || c.Method() == "HEAD") {
		closeFile()
		return c.Status(http.StatusNotModified).Send(nil)
	}
	contentType := mime.TypeByExtension(filepath.Ext(name))
	if contentType == "" {
		var prefix [512]byte
		n, err := file.Read(prefix[:])
		if err != nil && err != io.EOF {
			closeFile()
			return err
		}
		contentType = http.DetectContentType(prefix[:n])
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			closeFile()
			return err
		}
	}
	c.Set("Content-Type", contentType)
	c.Set("Accept-Ranges", "bytes")
	rangeHeader := c.Get("Range")
	if condition := c.Get("If-Range"); condition != "" {
		when, err := http.ParseTime(condition)
		if err != nil || !when.Equal(modified) {
			rangeHeader = ""
		}
	}
	var ranges []fiber.RangeSet
	if rangeHeader != "" && c.Method() != "HEAD" {
		parsed, err := c.Range(info.Size)
		if err != nil || parsed.Type != "bytes" || len(parsed.Ranges) == 0 {
			closeFile()
			c.Set("Content-Range", fmt.Sprintf("bytes */%d", info.Size))
			return c.SendStatus(http.StatusRequestedRangeNotSatisfiable)
		}
		ranges = parsed.Ranges
	}
	if c.Method() == "HEAD" {
		closeFile()
		c.Set("Content-Length", fmt.Sprint(info.Size))
		return c.Send(nil)
	}
	if len(ranges) == 0 {
		return c.SendStream(file, int(info.Size))
	}
	c.Status(http.StatusPartialContent)
	if len(ranges) == 1 {
		span := ranges[0]
		if _, err := file.Seek(span.Start, io.SeekStart); err != nil {
			closeFile()
			return err
		}
		c.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", span.Start, span.End, info.Size))
		return c.SendStream(&limitedDocument{Reader: io.LimitReader(file, span.End-span.Start+1), Closer: file}, int(span.End-span.Start+1))
	}
	// Multipart ranges stream from the same seekable storage reader; no whole-file buffer.
	boundary := multipart.NewWriter(io.Discard).Boundary()
	c.Set("Content-Type", "multipart/byteranges; boundary="+boundary)
	return c.SendStreamWriter(func(w *bufio.Writer) {
		defer closeFile()
		multipartWriter := multipart.NewWriter(w)
		if err := multipartWriter.SetBoundary(boundary); err != nil {
			log.Printf("[Storage] range boundary: %v", err)
			return
		}
		for _, span := range ranges {
			header := textproto.MIMEHeader{}
			header.Set("Content-Type", contentType)
			header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", span.Start, span.End, info.Size))
			part, err := multipartWriter.CreatePart(header)
			if err == nil {
				_, err = file.Seek(span.Start, io.SeekStart)
			}
			if err == nil {
				_, err = io.CopyN(part, file, span.End-span.Start+1)
			}
			if err != nil {
				log.Printf("[Storage] range stream: %v", err)
				return
			}
		}
		if err := multipartWriter.Close(); err != nil {
			log.Printf("[Storage] range close: %v", err)
		}
	})
}

type limitedDocument struct {
	io.Reader
	io.Closer
}
