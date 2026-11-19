# PANDA Backend

API Go untuk pengelolaan dokumen audit TI. API menerima upload dokumen, data folder tim, aplikasi, section versi, dan evidence. API menghasilkan audit log, pratinjau/unduhan dokumen, serta jawaban Agentic AI berbasis isi dokumen. Frontend pendamping: [fe-panda](https://github.com/athafawwaz/fe-panda).

Data dibatasi per tim dan branch (multi-tenant berbasis role). Startup menjalankan migrasi database. Chat AI dapat berjalan lama (write timeout 20 menit) dan mendukung streaming SSE.

> **Status: PoC, bukan konfigurasi produksi.** Login masih menerima password bypass bersama. Jangan mengekspos layanan ke internet atau memakai data sensitif sebelum autentikasi dan konfigurasi deployment diperkuat.

## Table of Contents

- [Requirements](#requirements)
- [Quick Start](#quick-start)
- [Environment Configuration](#environment-configuration)
- [Docker Compose](#docker-compose)
- [Authentication](#authentication)
- [API Overview](#api-overview)
- [Development](#development)
- [Panduan untuk AI Agent](#panduan-untuk-ai-agent)
- [Troubleshooting](#troubleshooting)
- [Sebelum Produksi](#sebelum-produksi)

## Requirements

Kebutuhan berbeda untuk jalur Docker penuh dan jalur API di host.

- Docker dengan Compose v2 (jalur Docker penuh, atau PostgreSQL saja).
- Go 1.26+ sesuai `go.mod`, hanya untuk menjalankan API di host atau menjalankan tes.
- PostgreSQL 17. Compose sudah menyediakannya; server lain juga dapat dipakai.
- Opsional: MinIO, LLM eksternal, dan reranker eksternal. Tanpa itu, file disimpan di `uploads/` dan AI memakai mode builtin.
- Cancellation chat memerlukan Linux dengan listener TCP/TLS. Platform lain gagal secara eksplisit.

## Quick Start

Jalankan semua perintah dari root repo backend. Mulai dengan file env:

```bash
cp .env.example .env
```

Isi dua secret wajib di `.env`. Contoh membuat nilai acak:

```bash
openssl rand -hex 24   # untuk DB_PASSWORD
openssl rand -hex 32   # untuk JWT_SECRET (minimal 32 karakter)
```

```text
DB_PASSWORD=<hasil openssl rand -hex 24>
JWT_SECRET=<hasil openssl rand -hex 32>
```

Pilih satu cara menjalankan API.

**A. Docker penuh (API + PostgreSQL):**

```bash
docker compose up -d --build --wait
```

**B. API di host, PostgreSQL di Docker:**

```bash
docker compose up -d --wait postgres
go run ./cmd/api
```

API mendengarkan di `http://127.0.0.1:8080/api/v1`. Cek awal:

```bash
curl -s http://127.0.0.1:8080/api/v1/public/stats
```

Endpoint ini membaca database, jadi ini bukan liveness check yang terpisah dari database.

**Startup mengubah database.** API menjalankan migrasi Goose, GORM AutoMigrate, dan seeding master tim/branch serta akun awal. Kegagalan langkah ini menghentikan startup. Gunakan database khusus pengembangan dan backup sebelum upgrade.

## Environment Configuration

Semua variabel dibaca dari environment proses, lalu dari `.env` di working directory, lalu `../.env`. Nilai environment proses selalu menang. Template lengkap ada di [`.env.example`](.env.example).

### Secret wajib

| Variable | Description |
|:---|:---|
| `DB_PASSWORD` | Password PostgreSQL. Kosong membuat startup gagal. Compose memakai nilai ini saat membuat volume baru. |
| `JWT_SECRET` | Penandatangan JWT dan sumber kunci enkripsi API key AI tersimpan. Minimal 32 karakter, tanpa `<` atau `>`. |

### Nilai runtime infrastruktur

| Variable | Default | Description |
|:---|---:|:---|
| `PORT` | `8080` | Port HTTP. Di Compose, juga port host untuk service `api`. |
| `DB_HOST` | `localhost` | Host PostgreSQL. `.env.example` memakai `127.0.0.1`. Compose menimpa menjadi `postgres` untuk service `api`. |
| `DB_PORT` | `5432` | Port PostgreSQL. |
| `DB_USER` | `panda_user` | User PostgreSQL. |
| `DB_NAME` | `panda` | Nama database. |
| `DB_SSLMODE` | `disable` | `disable` hanya untuk Docker lokal. Gunakan `verify-full` untuk server ber-TLS. |
| `UPLOAD_MAX_MB` | `25` | Ukuran maksimum per file, dalam MB. |
| `INITIAL_ADMIN_BADGE` | `2411249` | Badge akun Super Admin yang dibuat saat startup jika belum ada. |
| `BYPASS_PASSWORD` | `Panda` | Password bersama khusus PoC. Diterima untuk semua akun, tanpa beda huruf besar/kecil. |

### Integrasi opsional

| Variable | Default | Description |
|:---|---:|:---|
| `MINIO_ENDPOINT` | (kosong) | `host:port` atau URL lengkap. `https://` mengaktifkan SSL. |
| `MINIO_ACCESS_KEY` | (kosong) | Access key MinIO. |
| `MINIO_SECRET_KEY` | (kosong) | Secret key MinIO. |
| `MINIO_BUCKET` | (kosong) | Nama bucket. `.env.example` memakai `panda-documents`. |
| `MINIO_USE_SSL` | `false` | `true` atau `1` mengaktifkan TLS. |
| `MINIO_REGION` | (kosong) | Region MinIO. |
| `LLM_PROVIDER` | `builtin` | `builtin` atau provider OpenAI-compatible. Jika kosong dan `LLM_API_KEY` terisi, menjadi `custom`. |
| `LLM_BASE_URL` | `https://api.openai.com/v1` | Endpoint LLM. |
| `LLM_MODEL` | `gpt-4o-mini` | Model LLM. |
| `LLM_API_KEY` | (kosong) | API key LLM. |
| `LLM_SHARE_DOC_CONTENT` | `false` | Izin mengirim isi dokumen ke LLM eksternal. |
| `LLM_CHAT_HISTORY_TURNS` | `10` | Jumlah tanya-jawab terakhir yang dibaca AI, 0-30. |
| `LLM_THINKING_MAX_STEPS` | `0` | Batas langkah mode Thinking. `0` = tanpa batas, selain itu 2-40. |
| `RERANK_PROVIDER` | `none` | `none`, `cohere`, `jina`, `voyage`, atau `custom`. |
| `RERANK_BASE_URL` | (preset) | Kosong memakai preset provider. Wajib untuk `custom`. |
| `RERANK_MODEL` | (preset) | Kosong memakai preset provider. Wajib untuk `custom`. |
| `RERANK_API_KEY` | (kosong) | API key reranker. |
| `RERANK_CANDIDATES` | `30` | Jumlah kandidat, 5-50. |
| `RERANK_TOP_N` | `8` | Jumlah hasil, 1-8. |

Pengaturan LLM dan reranker yang disimpan dari menu Super Admin selalu menang atas environment.

`.env` dan `.env.*` sudah ada di `.gitignore`; jangan commit secret. Pada deployment, berikan nilai yang sama melalui secret platform, bukan file `.env` di image.

## Docker Compose

`compose.yaml` (project `panda-dev`) menjalankan PostgreSQL 17 dan API backend. Kedua port hanya terbuka di `127.0.0.1`.

```bash
docker compose up -d --build --wait
```

| Service | Port host | Volume | Catatan |
|:---|:---|:---|:---|
| `postgres` | `127.0.0.1:${DB_PORT:-5432}` | `panda-dev_postgres_data` | Health check `pg_isready`. |
| `api` | `127.0.0.1:${PORT:-8080}` | `panda-dev_uploads` → `/app/uploads` | Start setelah `postgres` sehat. Image distroless, user nonroot. |

Service `api` membaca `.env`, lalu menimpa `DB_HOST=postgres`, `DB_PORT=5432`, dan `PORT=8080` di dalam container.

```bash
docker compose ps
docker compose logs --tail 50 api
docker compose up -d --build api   # rebuild setelah perubahan kode
docker compose down                # data tetap ada di volume
```

Penting:

- **Jangan pakai `docker compose down -v`** kecuali memang ingin menghapus database dan file upload.
- Mengubah `DB_PASSWORD` di `.env` tidak mengganti password role yang sudah ada di volume.
- Build memakai `network: host`, jadi `go mod download` memakai DNS host. Resolver lokal seperti `127.0.0.53` tidak terlihat dari jaringan bridge Docker.
- User default image PostgreSQL adalah superuser untuk dev. Gunakan role terbatas untuk produksi.
- Platform deployment dapat memberikan variabel yang sama secara langsung tanpa Compose.

## Authentication

Login dengan badge dan password. Respons berisi `token` (JWT HS256, berlaku 72 jam) dan `user`.

```text
POST /api/v1/auth/login
Content-Type: application/json

{"badge_number":"2411249","password":"Panda"}
```

Kirim token di header untuk semua endpoint terproteksi:

```text
Authorization: Bearer <token>
```

Token di query string **tidak** diterima, termasuk untuk pratinjau/unduhan dokumen. Akses ke dokumen/aplikasi di luar scope mengembalikan `404`.

Super Admin dapat melihat data sebagai tim/branch lain (read-only, kecuali POST chat AI) dengan header:

```text
X-Impersonate-Team: <team_id>
X-Impersonate-Branch: <branch_id>
```

| Role | Akses |
|:---|:---|
| `SUPER_ADMIN` | Akses penuh, pengaturan AI/reranker, impersonation. |
| `ADMIN` | Semua tim, master data, dan folder. |
| `PEGAWAI_TI` | Tim/branch sendiri. Dokumen `ALL_TIM` terlihat lintas tim. |

**Rotasi `JWT_SECRET`** membatalkan token lama. API key LLM/reranker yang tersimpan juga perlu diisi ulang, karena dienkripsi dengan secret ini.

## API Overview

Error memakai satu bentuk JSON:

```json
{"error": "Pesan error dalam Bahasa Indonesia"}
```

Semua path relatif terhadap `/api/v1`.

| Group | Endpoints |
|:---|:---|
| Publik | `GET /public/stats`, `POST /auth/login` |
| Identitas | `GET /auth/me`, `GET /dashboard/summary` |
| Master tim | `GET /master/teams`, `POST /master/teams`, `PUT /master/teams/:id`, `DELETE /master/teams/:id` |
| Master branch | `POST /master/branches`, `PUT /master/branches/:id`, `DELETE /master/branches/:id` |
| Master user | `GET /master/users`, `POST /master/users`, `PUT /master/users/:id`, `DELETE /master/users/:id` |
| Folder dan aplikasi | `GET /folders`, `POST /folders`, `POST /folders/:id/applications`, `GET /applications/:id`, `POST /applications/:id/sections` |
| Dokumen | `GET /evidence`, `POST /documents/upload`, `GET /documents/:id/preview`, `GET /documents/:id/download` |
| Audit | `GET /logs` |
| Chat AI | `POST /ai/chat`, `POST /ai/chat/stream`, `GET /ai/chat/shortcuts`, `GET /ai/chat/sessions`, `GET /ai/chat/sessions/:id`, `DELETE /ai/chat/sessions/:id`, `GET /ai/quota` |
| Upload AI | `POST /ai/upload/analyze`, `POST /ai/upload/confirm`, `DELETE /ai/upload/:id` |
| Super Admin | `GET /admin/settings/ai`, `PUT /admin/settings/ai`, `POST /admin/settings/ai/test`, `GET /admin/settings/ai/quota`, `PUT /admin/settings/ai/quota`, `GET /admin/settings/rerank`, `PUT /admin/settings/rerank`, `POST /admin/settings/rerank/test` |

Tidak ada query parameter yang dipakai bersama oleh banyak endpoint. Sumber kebenaran route: [`cmd/api/main.go`](cmd/api/main.go). Handler tetap memeriksa role tambahan.

Perilaku penting:

- **Upload:** batas body `UPLOAD_MAX_MB` + 1 MiB sebelum parsing multipart. Ukuran dan isi file diperiksa lagi setelahnya. Label versi maksimal 50 karakter.
- **Download:** streaming, mendukung Range, HEAD, dan conditional request.
- **Chat SSE:** heartbeat 15 detik, event `answer_delta`/`answer_reset`, lalu satu event terminal `done` atau `error`. `done` dikirim hanya setelah penyimpanan berhasil.
- **Timeout server:** read 30 detik, idle 60 detik, write 20 menit. Reverse proxy harus mendukung respons lama.
- **Storage:** MinIO aktif jika endpoint, key, secret, dan bucket terisi. Jika MinIO aktif tetapi tidak dapat diakses, upload gagal; tidak ada fallback diam-diam ke lokal. Backup database **dan** file/bucket.

## Development

Perintah yang paling sering dipakai:

```bash
go test ./...
go test -race ./...
go vet ./...
go build -o bin/panda-api ./cmd/api
go test -race -tags integration ./internal/service -run Integration -count=1
```

Tes integration membuat database `panda_ragit_<uuid>` lalu menghapusnya; akun DB memerlukan hak `CREATEDB`. `PANDA_IT_DB_NAME` hanya boleh menunjuk database test kosong. Evaluasi retrieval: [`eval/README.md`](eval/README.md).

Auto reload dengan Air (dipasang di level user, bukan dependency aplikasi):

```bash
GOBIN="$HOME/.local/bin" go install github.com/air-verse/air@v1.67.4
"$HOME/.local/bin/air" --tmp_dir bin \
  --build.cmd 'go build -o ./bin/panda-api ./cmd/api' \
  --build.entrypoint ./bin/panda-api \
  --build.exclude_dir 'bin,uploads,.git' \
  --build.include_ext 'go,sql' --build.include_file '.env' \
  --build.stop_on_error true
```

Migrasi SQL ada di `internal/database/migrations/` (Goose, ter-embed ke binary).

```text
cmd/api/             Entrypoint HTTP dan registrasi route
cmd/rag-eval/        CLI evaluasi retrieval
internal/handler/    HTTP, otorisasi, upload, dan chat
internal/service/    AI/RAG, scope data, dan konfigurasi
internal/storage/    File lokal dan MinIO
internal/database/   Koneksi, migrasi SQL, dan seeding
internal/models/     Model data
```

## Panduan untuk AI Agent

Bagian ini untuk coding agent (Claude Code, pi, Codex, dan sejenisnya) yang bekerja di repo ini. Ikuti urutan dan aturan berikut.

### Aturan keras

1. **Jangan baca, tampilkan, atau commit `.env`.** Pakai `.env.example` sebagai referensi nama variabel. Untuk nilai secret, minta user atau gunakan tool env yang disetujui user.
2. **Jangan jalankan `docker compose down -v`** atau `DROP DATABASE` tanpa persetujuan eksplisit. Volume berisi data dev dan file upload.
3. **Jangan jalankan tes integration ke database dev.** Tes ini menulis data; pakai database unik bawaan tes.
4. **Jangan ubah `JWT_SECRET` atau `DB_PASSWORD`** di environment yang sudah berisi data. Lihat catatan rotasi di [Authentication](#authentication) dan [Docker Compose](#docker-compose).
5. Jalankan semua perintah dari root repo backend, karena `.env` dan `uploads/` relatif terhadap working directory.

### Menjalankan dan memverifikasi

| Tujuan | Perintah |
|:---|:---|
| Menjalankan stack penuh | `docker compose up -d --build --wait` |
| Menjalankan API di host | `docker compose up -d --wait postgres`, lalu `go run ./cmd/api` sebagai proses latar |
| Smoke test | `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/api/v1/public/stats` harus `200` |
| Melihat log | `docker compose logs --tail 50 api` |
| Verifikasi sebelum commit | `go vet ./...` lalu `go test ./...` |

`go run ./cmd/api` dan `air` tidak pernah selesai. Jalankan lewat tool proses latar milik agent (misalnya `proc` dengan `action: "start"`, `cwd` = root backend), bukan `bash` dengan `&` atau `sleep`. Simpan handle yang dikembalikan untuk `logs` dan `stop`.

### Peta perubahan

| Jika mengubah | Juga perbarui |
|:---|:---|
| Route di `cmd/api/main.go` | Tes `cmd/api/authorization_test.go` dan `cmd/api/http_contract_test.go`, tabel [API Overview](#api-overview) |
| Skema database | File migrasi baru di `internal/database/migrations/` dengan nomor berikutnya (`00011_...sql`). Jangan edit migrasi yang sudah ada. |
| Variabel env baru | `.env.example` (dengan komentar) dan tabel [Environment Configuration](#environment-configuration) |
| Ekstraksi, chunking, atau pencarian RAG | Bandingkan dengan baseline melalui [`eval/README.md`](eval/README.md) |
| `Dockerfile` atau `compose.yaml` | `docker compose config -q`, lalu build dan smoke test |

### Konvensi

- Pesan error API dan komentar kode memakai Bahasa Indonesia. Pertahankan gaya ini.
- Handler memakai `func(fiber.Ctx) error` (GoFiber v3). Jangan teruskan `fiber.Ctx` ke service atau goroutine; salin nilai yang dibutuhkan.
- Error HTTP selalu `{"error":"..."}`.
- Akses dokumen di luar scope mengembalikan `404`, bukan `403`, agar keberadaan data tidak bocor.
- Gunakan dependency yang sudah ada di `go.mod` sebelum menambah yang baru.

## Troubleshooting

| Gejala | Pemeriksaan |
|:---|:---|
| `Invalid JWT configuration` | Isi `JWT_SECRET` acak minimal 32 karakter, tanpa placeholder. |
| `DB_PASSWORD belum diisi` / koneksi PostgreSQL gagal | Periksa `.env`, `docker compose ps`, working directory, user/database, dan port `5432`. |
| Build Docker gagal di `go mod download` | Periksa DNS host. Build sudah memakai `network: host`; pastikan host dapat me-resolve `proxy.golang.org`. |
| `module lookup disabled by GOPROXY=off` | Modul belum ada di cache. Izinkan akses registry lalu jalankan `go mod download`. |
| Dokumen `401` / `404` | Periksa Bearer token, masa berlaku, dan scope tim/branch. URL dokumen bukan tautan publik. |
| Peringatan MinIO saat startup | Lengkapi kredensial dan cek bucket/TLS/jaringan. Kosongkan kredensial hanya jika memilih storage lokal. |
| Upload versi ditolak | Label versi maksimal 50 karakter, tanpa pemisah path atau karakter khusus nama file. |

## Sebelum Produksi

Langkah berikut wajib sebelum layanan dipakai di produksi.

- Cabut bypass login dan penyimpanan password PoC.
- Gunakan HTTPS dan batasi akses jaringan. Tinjau CORS dan TLS PostgreSQL.
- Gunakan role PostgreSQL terbatas, bukan superuser Docker dev.
- Audit dependency dan verifikasi isolasi tim/branch dengan tes end-to-end.
- Port ini untuk database PostgreSQL baru. Data SQL Server lama memerlukan migrasi terpisah.
