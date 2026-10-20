// Package storage menyimpan file dokumen PANDA. MinIO dipakai otomatis bila MINIO_ENDPOINT, MINIO_ACCESS_KEY,
// MINIO_SECRET_KEY, dan MINIO_BUCKET terisi; bila tidak, file disimpan di folder lokal `uploads/` seperti sebelumnya.
//
// Lokasi yang disimpan di kolom documents.file_path / upload_staging.file_path menentukan tempat file berada:
// `minio://<bucket>/<key>` untuk objek MinIO, path biasa untuk file lokal. Karena itu dokumen lama di folder lokal
// tetap bisa dibuka walau MinIO baru diaktifkan.
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	DriverLocal = "local"
	DriverMinIO = "minio"

	minioScheme  = "minio://"
	opTimeout    = 60 * time.Second
	checkTimeout = 10 * time.Second
)

// ErrNotFound: file atau objek tidak ada di storage.
var ErrNotFound = errors.New("file tidak ditemukan di storage")

// Config adalah konfigurasi storage dari environment.
type Config struct {
	LocalRoot string // folder file lokal (default "uploads")
	Endpoint  string // host:port MinIO, boleh diawali http:// atau https://
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool
	Region    string
}

// MinIOEnabled: MinIO dipakai bila endpoint, bucket, access key, dan secret key semuanya terisi.
func (c Config) MinIOEnabled() bool {
	return strings.TrimSpace(c.Endpoint) != "" && strings.TrimSpace(c.Bucket) != "" &&
		strings.TrimSpace(c.AccessKey) != "" && strings.TrimSpace(c.SecretKey) != ""
}

// ConfigFromEnv membaca MINIO_ENDPOINT, MINIO_ACCESS_KEY, MINIO_SECRET_KEY, MINIO_BUCKET, MINIO_USE_SSL, MINIO_REGION.
func ConfigFromEnv() Config {
	ssl := strings.TrimSpace(os.Getenv("MINIO_USE_SSL"))
	return Config{
		LocalRoot: "uploads",
		Endpoint:  strings.TrimSpace(os.Getenv("MINIO_ENDPOINT")),
		AccessKey: strings.TrimSpace(os.Getenv("MINIO_ACCESS_KEY")),
		SecretKey: strings.TrimSpace(os.Getenv("MINIO_SECRET_KEY")),
		Bucket:    strings.TrimSpace(os.Getenv("MINIO_BUCKET")),
		UseSSL:    strings.EqualFold(ssl, "true") || ssl == "1",
		Region:    strings.TrimSpace(os.Getenv("MINIO_REGION")),
	}
}

// parseEndpoint menerima "host:port" atau URL lengkap; skema https mengaktifkan SSL.
func parseEndpoint(raw string, useSSL bool) (string, bool, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		return strings.TrimRight(raw, "/"), useSSL, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false, fmt.Errorf("MINIO_ENDPOINT tidak valid: %q", raw)
	}
	return u.Host, u.Scheme == "https", nil
}

type store struct {
	cfg    Config
	client *minio.Client // nil = MinIO tidak aktif
}

var (
	mu     sync.RWMutex
	active = &store{cfg: Config{LocalRoot: "uploads"}}
)

func current() *store {
	mu.RLock()
	defer mu.RUnlock()
	return active
}

// Init memasang konfigurasi storage. Bila MinIO aktif, koneksi & bucket dicek (bucket dibuat bila belum ada).
// Kegagalan cek hanya dilaporkan: MinIO tetap dipakai agar file tidak diam-diam tersimpan di tempat lain, dan
// upload akan gagal dengan pesan jelas sampai MinIO dapat dihubungi.
func Init(cfg Config) error {
	if cfg.LocalRoot == "" {
		cfg.LocalRoot = "uploads"
	}
	s := &store{cfg: cfg}
	var checkErr error
	if cfg.MinIOEnabled() {
		endpoint, secure, err := parseEndpoint(cfg.Endpoint, cfg.UseSSL)
		if err != nil {
			return err
		}
		client, err := minio.New(endpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
			Secure: secure,
			Region: cfg.Region,
		})
		if err != nil {
			return fmt.Errorf("klien MinIO gagal dibuat: %w", err)
		}
		s.client = client
		checkErr = s.ensureBucket()
	}
	mu.Lock()
	active = s
	mu.Unlock()
	return checkErr
}

func (s *store) ensureBucket() error {
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	exists, err := s.client.BucketExists(ctx, s.cfg.Bucket)
	if err != nil {
		return fmt.Errorf("MinIO %s tidak dapat dihubungi: %w", s.cfg.Endpoint, err)
	}
	if exists {
		return nil
	}
	if err := s.client.MakeBucket(ctx, s.cfg.Bucket, minio.MakeBucketOptions{Region: s.cfg.Region}); err != nil {
		return fmt.Errorf("bucket MinIO %q belum ada dan gagal dibuat: %w", s.cfg.Bucket, err)
	}
	log.Printf("[Storage] bucket MinIO %q dibuat", s.cfg.Bucket)
	return nil
}

// Driver mengembalikan storage yang dipakai untuk file baru: "minio" atau "local".
func Driver() string {
	if current().client != nil {
		return DriverMinIO
	}
	return DriverLocal
}

// Describe menjelaskan storage aktif untuk log (tanpa kredensial).
func Describe() string {
	s := current()
	if s.client == nil {
		return fmt.Sprintf("lokal (folder %s)", s.cfg.LocalRoot)
	}
	return fmt.Sprintf("MinIO %s, bucket %s", s.cfg.Endpoint, s.cfg.Bucket)
}

// IsMinIOLocation: lokasi menunjuk objek MinIO.
func IsMinIOLocation(location string) bool { return strings.HasPrefix(location, minioScheme) }

func normalizeKey(key string) string {
	return strings.TrimLeft(strings.ReplaceAll(key, "\\", "/"), "/")
}

// splitMinIO memecah "minio://bucket/key" menjadi bucket & key.
func splitMinIO(location string) (string, string, error) {
	rest := strings.TrimPrefix(location, minioScheme)
	i := strings.IndexByte(rest, '/')
	if i <= 0 || i == len(rest)-1 {
		return "", "", fmt.Errorf("lokasi MinIO tidak valid: %q", location)
	}
	return rest[:i], rest[i+1:], nil
}

func (s *store) minioFor(location string) (string, string, error) {
	if s.client == nil {
		return "", "", fmt.Errorf("file tersimpan di MinIO (%s) tetapi kredensial MinIO belum diisi", location)
	}
	return splitMinIO(location)
}

func contentTypeFor(key string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(key))); t != "" {
		return t
	}
	return "application/octet-stream"
}

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, opTimeout)
}

func isNoSuchKey(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket" || resp.StatusCode == http.StatusNotFound
}

// Exists: file dengan key relatif (mis. "STD.pdf", "_staging/x.pdf") sudah ada di storage aktif.
func Exists(ctx context.Context, key string) (bool, error) {
	s := current()
	key = normalizeKey(key)
	if s.client == nil {
		_, err := os.Stat(filepath.Join(s.cfg.LocalRoot, filepath.FromSlash(key)))
		if err == nil {
			return true, nil
		}
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := s.client.StatObject(ctx, s.cfg.Bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if isNoSuchKey(err) {
		return false, nil
	}
	return false, err
}

// Save menyimpan file ke storage aktif dan mengembalikan lokasi yang disimpan di database.
func Save(ctx context.Context, key string, data []byte) (string, error) {
	s := current()
	key = normalizeKey(key)
	if s.client == nil {
		path := filepath.Join(s.cfg.LocalRoot, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return "", fmt.Errorf("folder penyimpanan tidak dapat dibuat: %w", err)
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			return "", err
		}
		return path, nil
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if _, err := s.client.PutObject(ctx, s.cfg.Bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentTypeFor(key)}); err != nil {
		return "", fmt.Errorf("upload ke MinIO gagal: %w", err)
	}
	return minioScheme + s.cfg.Bucket + "/" + key, nil
}

// Read membaca seluruh isi file dari lokasi (lokal atau MinIO).
func Read(ctx context.Context, location string) ([]byte, error) {
	rc, _, err := Open(ctx, location)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Info adalah ukuran & waktu ubah file.
type Info struct {
	Size    int64
	ModTime time.Time
}

// ReadSeekCloser dipakai untuk melayani preview/download (mendukung HTTP Range).
type ReadSeekCloser interface {
	io.ReadSeeker
	io.Closer
}

// Open membuka file dari lokasi untuk dibaca bertahap. ErrNotFound bila file/objek tidak ada.
func Open(ctx context.Context, location string) (ReadSeekCloser, Info, error) {
	s := current()
	if !IsMinIOLocation(location) {
		f, err := os.Open(location)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, Info{}, ErrNotFound
			}
			return nil, Info{}, err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, Info{}, err
		}
		return f, Info{Size: st.Size(), ModTime: st.ModTime()}, nil
	}
	bucket, key, err := s.minioFor(location)
	if err != nil {
		return nil, Info{}, err
	}
	// Objek dibaca bertahap selama response dikirim, jadi tidak memakai batas waktu operasi pendek
	obj, err := s.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, Info{}, err
	}
	st, err := obj.Stat()
	if err != nil {
		obj.Close()
		if isNoSuchKey(err) {
			return nil, Info{}, ErrNotFound
		}
		return nil, Info{}, err
	}
	return obj, Info{Size: st.Size, ModTime: st.LastModified}, nil
}

// Remove menghapus file di lokasi (lokal atau MinIO). File yang sudah tidak ada dianggap berhasil.
func Remove(ctx context.Context, location string) error {
	if location == "" {
		return nil
	}
	s := current()
	if !IsMinIOLocation(location) {
		if err := os.Remove(location); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	bucket, key, err := s.minioFor(location)
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if err := s.client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{}); err != nil && !isNoSuchKey(err) {
		return err
	}
	return nil
}
