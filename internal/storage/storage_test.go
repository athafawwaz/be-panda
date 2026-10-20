package storage

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 adalah server S3 minimal (path-style) untuk operasi yang dipakai PANDA: cek/buat bucket, simpan
// (termasuk body aws-chunked dari minio-go), baca dengan Range, stat, dan hapus objek.
type fakeS3 struct {
	mu      sync.Mutex
	buckets map[string]map[string][]byte
	puts    int
}

func newFakeS3(t *testing.T) (*fakeS3, *httptest.Server) {
	f := &fakeS3{buckets: map[string]map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

// decodeAWSChunked membaca body "STREAMING-*" ("<hex>;chunk-signature=...\r\n<data>\r\n ... 0;...\r\n<trailer>").
func decodeAWSChunked(body io.Reader) ([]byte, error) {
	r := bufio.NewReader(body)
	var out bytes.Buffer
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.ParseInt(strings.TrimSpace(strings.SplitN(line, ";", 2)[0]), 16, 64)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			return out.Bytes(), nil
		}
		if _, err := io.CopyN(&out, r, size); err != nil {
			return nil, err
		}
		if _, err := r.ReadString('\n'); err != nil {
			return nil, err
		}
	}
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	bucket, key := parts[0], ""
	if len(parts) == 2 {
		key = parts[1]
	}
	objects, bucketExists := f.buckets[bucket]
	notFound := func(code string) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		if r.Method != http.MethodHead {
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>not found</Message></Error>`, code)
		}
	}
	if key == "" {
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Has("location"):
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
		case r.Method == http.MethodHead && !bucketExists:
			notFound("NoSuchBucket")
		case r.Method == http.MethodPut:
			f.buckets[bucket] = map[string][]byte{}
		}
		return
	}
	if !bucketExists {
		notFound("NoSuchBucket")
		return
	}
	switch r.Method {
	case http.MethodPut:
		var data []byte
		var err error
		if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			data, err = decodeAWSChunked(r.Body)
		} else {
			data, err = io.ReadAll(r.Body)
		}
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		objects[key] = data
		f.puts++
		sum := md5.Sum(data)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	case http.MethodGet, http.MethodHead:
		data, ok := objects[key]
		if !ok {
			notFound("NoSuchKey")
			return
		}
		sum := md5.Sum(data)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		w.Header().Set("Last-Modified", time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC).Format(http.TimeFormat))
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Accept-Ranges", "bytes")
		start, end := 0, len(data)-1
		status := http.StatusOK
		if rng := r.Header.Get("Range"); strings.HasPrefix(rng, "bytes=") && r.Method == http.MethodGet {
			spec := strings.SplitN(strings.TrimPrefix(rng, "bytes="), "-", 2)
			start, _ = strconv.Atoi(spec[0])
			if spec[1] != "" {
				end, _ = strconv.Atoi(spec[1])
			}
			end = min(end, len(data)-1)
			status = http.StatusPartialContent
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		}
		w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		w.WriteHeader(status)
		if r.Method == http.MethodGet {
			w.Write(data[start : end+1])
		}
	case http.MethodDelete:
		delete(objects, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

func resetStore(t *testing.T) {
	t.Cleanup(func() { _ = Init(Config{LocalRoot: "uploads"}) })
}

func TestMinIOEnabledOnlyWithCompleteCredentials(t *testing.T) {
	full := Config{Endpoint: "localhost:9000", AccessKey: "ak", SecretKey: "sk", Bucket: "panda-documents"}
	if !full.MinIOEnabled() {
		t.Fatalf("kredensial lengkap harus mengaktifkan MinIO")
	}
	for _, c := range []Config{
		{Endpoint: "localhost:9000", Bucket: "panda-documents"}, // kondisi .env saat ini: key kosong
		{Endpoint: "localhost:9000", AccessKey: "ak", Bucket: "panda-documents"},
		{AccessKey: "ak", SecretKey: "sk", Bucket: "panda-documents"},
		{Endpoint: "localhost:9000", AccessKey: "ak", SecretKey: "sk"},
		{Endpoint: " ", AccessKey: "ak", SecretKey: "sk", Bucket: "b"},
	} {
		if c.MinIOEnabled() {
			t.Errorf("konfigurasi tidak lengkap tidak boleh mengaktifkan MinIO: %+v", c)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("MINIO_ENDPOINT", " localhost:9000 ")
	t.Setenv("MINIO_ACCESS_KEY", "ak")
	t.Setenv("MINIO_SECRET_KEY", "sk")
	t.Setenv("MINIO_BUCKET", "panda-documents")
	t.Setenv("MINIO_USE_SSL", "TRUE")
	t.Setenv("MINIO_REGION", "us-east-1")
	c := ConfigFromEnv()
	if !c.MinIOEnabled() || c.Endpoint != "localhost:9000" || !c.UseSSL || c.Region != "us-east-1" || c.LocalRoot != "uploads" {
		t.Fatalf("konfigurasi dari env salah: %+v", c)
	}
}

func TestParseEndpoint(t *testing.T) {
	cases := []struct {
		raw    string
		ssl    bool
		host   string
		secure bool
	}{
		{"localhost:9000", false, "localhost:9000", false},
		{"minio.pusri.co.id", true, "minio.pusri.co.id", true},
		{"http://localhost:9000/", true, "localhost:9000", false},
		{"https://minio.pusri.co.id", false, "minio.pusri.co.id", true},
	}
	for _, c := range cases {
		host, secure, err := parseEndpoint(c.raw, c.ssl)
		if err != nil || host != c.host || secure != c.secure {
			t.Errorf("%q: dapat %q %v %v", c.raw, host, secure, err)
		}
	}
}

func TestLocalStorageRoundTrip(t *testing.T) {
	resetStore(t)
	root := t.TempDir()
	if err := Init(Config{LocalRoot: root}); err != nil {
		t.Fatal(err)
	}
	if Driver() != DriverLocal {
		t.Fatalf("tanpa kredensial MinIO harus lokal")
	}
	ctx := context.Background()
	loc, err := Save(ctx, "_staging/abc.pdf", []byte("%PDF-isi"))
	if err != nil || loc != filepath.Join(root, "_staging", "abc.pdf") || IsMinIOLocation(loc) {
		t.Fatalf("lokasi lokal salah: %q %v", loc, err)
	}
	if ok, _ := Exists(ctx, "_staging/abc.pdf"); !ok {
		t.Fatalf("file lokal harus terdeteksi ada")
	}
	if data, err := Read(ctx, loc); err != nil || string(data) != "%PDF-isi" {
		t.Fatalf("isi file lokal salah: %q %v", data, err)
	}
	if err := Remove(ctx, loc); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(ctx, loc); !errors.Is(err, ErrNotFound) {
		t.Fatalf("file yang dihapus harus ErrNotFound: %v", err)
	}
	if err := Remove(ctx, loc); err != nil {
		t.Fatalf("menghapus file yang sudah tidak ada tidak boleh error: %v", err)
	}
}

func TestMinIOStorageRoundTrip(t *testing.T) {
	resetStore(t)
	fake, srv := newFakeS3(t)
	root := t.TempDir()
	err := Init(Config{LocalRoot: root, Endpoint: srv.URL, AccessKey: "ak", SecretKey: "sk", Bucket: "panda-documents", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("Init MinIO gagal: %v", err)
	}
	if _, ok := fake.buckets["panda-documents"]; !ok || Driver() != DriverMinIO {
		t.Fatalf("bucket yang belum ada harus dibuat & driver MinIO aktif")
	}
	ctx := context.Background()
	content := []byte("%PDF-1.7 berita acara serah terima CorePay v1.1")
	loc, err := Save(ctx, "VISTA_BAST_v1.1_20261020.pdf", content)
	if err != nil || loc != "minio://panda-documents/VISTA_BAST_v1.1_20261020.pdf" {
		t.Fatalf("lokasi MinIO salah: %q %v", loc, err)
	}
	if !bytes.Equal(fake.buckets["panda-documents"]["VISTA_BAST_v1.1_20261020.pdf"], content) {
		t.Fatalf("isi objek di MinIO berbeda: %q", fake.buckets["panda-documents"]["VISTA_BAST_v1.1_20261020.pdf"])
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("saat MinIO aktif, file tidak boleh ditulis ke folder lokal")
	}
	if ok, err := Exists(ctx, "VISTA_BAST_v1.1_20261020.pdf"); !ok || err != nil {
		t.Fatalf("objek harus terdeteksi ada: %v", err)
	}
	if ok, err := Exists(ctx, "tidak-ada.pdf"); ok || err != nil {
		t.Fatalf("objek yang tidak ada: ok=%v err=%v", ok, err)
	}
	if data, err := Read(ctx, loc); err != nil || !bytes.Equal(data, content) {
		t.Fatalf("baca dari MinIO salah: %q %v", data, err)
	}

	// Preview/download memakai http.ServeContent: Range harus dilayani dari objek MinIO
	rc, info, err := Open(ctx, loc)
	if err != nil || info.Size != int64(len(content)) {
		t.Fatalf("Open MinIO salah: %+v %v", info, err)
	}
	req := httptest.NewRequest(http.MethodGet, "/preview", nil)
	req.Header.Set("Range", "bytes=0-7")
	rec := httptest.NewRecorder()
	http.ServeContent(rec, req, "VISTA_BAST.pdf", info.ModTime, rc)
	rc.Close()
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "%PDF-1.7" {
		t.Fatalf("Range dari MinIO salah: %d %q", rec.Code, rec.Body.String())
	}

	if err := Remove(ctx, loc); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(ctx, loc); !errors.Is(err, ErrNotFound) {
		t.Fatalf("objek yang dihapus harus ErrNotFound: %v", err)
	}
}

func TestLegacyLocalFilesStillReadableWithMinIO(t *testing.T) {
	resetStore(t)
	_, srv := newFakeS3(t)
	legacy := filepath.Join(t.TempDir(), "LAMA_SOP_v1.0.pdf")
	if err := os.WriteFile(legacy, []byte("dokumen lama"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Init(Config{Endpoint: srv.URL, AccessKey: "ak", SecretKey: "sk", Bucket: "panda-documents", Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	if data, err := Read(context.Background(), legacy); err != nil || string(data) != "dokumen lama" {
		t.Fatalf("dokumen lama di folder lokal harus tetap terbaca setelah MinIO aktif: %q %v", data, err)
	}
}

func TestMinIOLocationWithoutCredentials(t *testing.T) {
	resetStore(t)
	if err := Init(Config{LocalRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	_, err := Read(context.Background(), "minio://panda-documents/X.pdf")
	if err == nil || !strings.Contains(err.Error(), "kredensial MinIO belum diisi") {
		t.Fatalf("dokumen di MinIO tanpa kredensial harus dijelaskan: %v", err)
	}
}

func TestInitReportsUnreachableMinIO(t *testing.T) {
	resetStore(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // alamat yang tidak lagi melayani
	err := Init(Config{Endpoint: url, AccessKey: "ak", SecretKey: "sk", Bucket: "b", Region: "us-east-1"})
	if err == nil || Driver() != DriverMinIO {
		t.Fatalf("MinIO yang tidak bisa dihubungi harus dilaporkan tanpa diam-diam pindah ke lokal: driver=%s err=%v", Driver(), err)
	}
}
