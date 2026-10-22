# PANDA — Backend

API pengelolaan dokumen audit TI: folder tim, aplikasi dan section versi, evidence, audit log, serta pencarian Agentic AI berbasis isi dokumen. Frontend pendamping: [fe-panda](https://github.com/athafawwaz/fe-panda).

> **Status: PoC, bukan konfigurasi produksi.** Login masih menerima password bypass bersama. Jangan mengekspos layanan ke internet atau memakai data sensitif sebelum autentikasi dan konfigurasi deployment diperkuat.

## Stack dan kebutuhan

- Go sesuai `go.mod`. Untuk auto reload, Air yang dipasang sekarang membutuhkan Go 1.25+.
- Docker dengan Compose untuk PostgreSQL 17 dev, atau server PostgreSQL 17 yang sudah tersedia.
- Hak perubahan skema pada database pengembangan untuk migrasi saat startup.
- MinIO opsional. Tanpa kredensial lengkap, file baru disimpan di `uploads/`.
- LLM dan reranker eksternal opsional; mode builtin dapat dipakai tanpa API key eksternal.

## Mulai cepat

Jalankan seluruh perintah berikut dari root repo backend.

```bash
cp .env.example .env
go mod download
```

Edit `.env` sebelum menjalankan API:

1. Isi `DB_PASSWORD` acak. Default Compose: `DB_HOST=127.0.0.1`, `DB_PORT=5432`, `DB_USER=panda_user`, `DB_NAME=panda`, dan `DB_SSLMODE=disable` (khusus lokal). Compose membuat user/database pada volume baru.
2. Ganti `JWT_SECRET` dengan nilai acak minimal 32 karakter, bukan placeholder. Contoh membuat nilai acak menggunakan Node.js:

   ```bash
   node -e 'console.log(require("node:crypto").randomBytes(32).toString("hex"))'
   ```

3. Simpan hasilnya sebagai `JWT_SECRET` di `.env`. Jangan masukkan secret ke Git.

```bash
docker compose up -d --wait postgres
go run ./cmd/api
```

API tersedia di `http://localhost:8080/api/v1`, kecuali `PORT` diubah. Endpoint publik untuk pengecekan awal: `GET /api/v1/public/stats`; endpoint ini membaca database, bukan liveness check yang independen dari database.

**Startup mengubah database.** API menjalankan migrasi Goose, GORM AutoMigrate, dan seeding master tim/branch serta akun awal. Kegagalan migrasi/sinkronisasi/seed menghentikan startup. Gunakan database khusus pengembangan dan backup sebelum upgrade.

### Database Docker lokal

`compose.yaml` hanya menjalankan PostgreSQL; BE/FE tetap dijalankan di host. Port DB dibatasi ke `127.0.0.1`. Data disimpan di named volume `panda-dev_postgres_data`.

```bash
docker compose ps
docker compose logs --tail 50 postgres
docker compose stop postgres
docker compose up -d --wait postgres
```

`docker compose down` mempertahankan volume. **Jangan tambah `-v` kecuali memang ingin menghapus seluruh data.** Mengubah `DB_PASSWORD` pada `.env` tidak mengganti password role yang sudah ada di volume. Default user image Docker adalah superuser untuk dev; gunakan role terbatas untuk produksi.

Port ini ditujukan untuk **database PostgreSQL baru**, bukan migrasi isi SQL Server lama. Backup/export dan transfer data SQL Server memerlukan langkah terpisah.

### Auto reload backend

Pasang Air pada user (tidak mengubah dependency aplikasi), kemudian jalankan dari root backend:

```bash
GOBIN="$HOME/.local/bin" go install github.com/air-verse/air@v1.67.4
"$HOME/.local/bin/air" --tmp_dir bin \
  --build.cmd 'go build -o ./bin/panda-api ./cmd/api' \
  --build.entrypoint ./bin/panda-api \
  --build.exclude_dir 'bin,uploads,.git' \
  --build.include_ext 'go,sql' --build.include_file '.env' \
  --build.stop_on_error true
```

### Menjalankan lewat tool `proc`

Bila menggunakan agent dengan tool `proc`, panggil dari agent, bukan sebagai perintah shell:

```json
{
  "action": "start",
  "command": "go run ./cmd/api",
  "cwd": "/path/to/backend",
  "name": "PANDA backend"
}
```

Simpan handle `proc-*` yang dikembalikan. Gunakan `action: "logs"` atau `action: "stop"` dengan `handle` tersebut untuk melihat log atau menghentikan proses.

## Konfigurasi

Daftar lengkap dan nilai contoh ada di [`.env.example`](.env.example). Environment proses mengalahkan nilai `.env`; aplikasi mencoba memuat `.env` di working directory lalu `../.env`. Jalankan dari root backend agar lokasi file konsisten.

| Variabel | Kegunaan |
| --- | --- |
| `PORT` | Port HTTP; default `8080`. |
| `DB_HOST`, `DB_PORT`, `DB_USER`, `DB_PASSWORD`, `DB_NAME` | Koneksi PostgreSQL; default lokal `127.0.0.1:5432`, user `panda_user`, database `panda`. Password wajib diisi. |
| `DB_SSLMODE` | `disable` untuk Docker lokal, `verify-full` untuk server remote dengan sertifikat tepercaya. |
| `JWT_SECRET` | Penandatangan JWT sekaligus sumber kunci enkripsi API key AI tersimpan. Wajib acak, minimal 32 karakter, tanpa placeholder `<...>`. |
| `INITIAL_ADMIN_BADGE` | Badge akun Super Admin awal; default PoC `2411249`. Startup memastikan badge ini memiliki role Super Admin. |
| `BYPASS_PASSWORD` | Password login bersama khusus PoC; default `Panda`. Bukan password produksi. |
| `UPLOAD_MAX_MB` | Batas ukuran file upload; contoh `25` MB. |
| `MINIO_ENDPOINT`, `MINIO_ACCESS_KEY`, `MINIO_SECRET_KEY`, `MINIO_BUCKET` | Mengaktifkan penyimpanan MinIO bila seluruhnya terisi. |
| `MINIO_USE_SSL`, `MINIO_REGION` | TLS dan region MinIO. |
| `LLM_PROVIDER`, `LLM_BASE_URL`, `LLM_MODEL`, `LLM_API_KEY` | Provider/model AI. Pengaturan tersimpan melalui menu Super Admin diprioritaskan atas konfigurasi environment. |
| `LLM_SHARE_DOC_CONTENT` | Izin mengirim isi dokumen ke provider AI eksternal; contoh `false`. |
| `RERANK_PROVIDER`, `RERANK_BASE_URL`, `RERANK_MODEL`, `RERANK_API_KEY` | Reranker opsional; contoh provider `none`. |

### Penyimpanan dan pekerjaan latar

- Lokal: file dan staging berada di `uploads/`, relatif terhadap working directory. Jangan commit direktori ini.
- MinIO: ketika diaktifkan tetapi tidak dapat diakses, operasi upload gagal; layanan tidak diam-diam berpindah ke lokal. Dokumen lokal lama tetap dapat dibaca.
- Startup menjalankan pengindeksan dokumen yang tertunda dan pembersihan staging upload kedaluwarsa di latar.
- Backup database **dan** file lokal/bucket MinIO. Backup database saja tidak mencakup isi dokumen.

## Login dan aturan akses

Untuk database baru dengan konfigurasi contoh, akun awal menggunakan badge `2411249` dan password bypass `Panda`. Akun harus terdaftar dan aktif. Mengubah `BYPASS_PASSWORD` saja **bukan** migrasi password akun yang sudah tersimpan.

- `SUPER_ADMIN`: akses penuh, pengaturan AI, dan impersonation segmen.
- `ADMIN`: akses seluruh tim serta pengelolaan master data/folder.
- `PEGAWAI_TI`: dibatasi tim/branch, dengan dokumen `ALL_TIM` dapat dilihat lintas tim.
- Impersonation bersifat read-only; chat AI dan streaming merupakan pengecualian POST yang diizinkan.

API terproteksi menerima `Authorization: Bearer <token>`. Token query string tidak diterima. JWT wajib HS256 dan memiliki `exp`. Preview/download juga memerlukan Bearer token dan pemeriksaan scope; akses dokumen/aplikasi di luar scope mengembalikan `404`.

**Rotasi `JWT_SECRET`:** token lama tidak berlaku lagi. Karena secret juga dipakai untuk enkripsi pengaturan, API key LLM/reranker tersimpan dapat perlu diisi ulang. Rencanakan rotasi, jangan menggantinya sembarangan saat restart.

## Peta endpoint

Semua path di bawah relatif terhadap `/api/v1`.

| Area | Endpoint utama |
| --- | --- |
| Publik | `GET /public/stats`, `POST /auth/login` |
| Identitas/dashboard | `GET /auth/me`, `GET /dashboard/summary` |
| Master akses | `/master/teams`, `/master/branches`, `/master/users` |
| Aplikasi | `/folders`, `/folders/:id/applications`, `/applications/:id`, `/applications/:id/sections` |
| Dokumen | `GET /evidence`, `POST /documents/upload`, `GET /documents/:id/preview`, `GET /documents/:id/download` |
| Audit | `GET /logs` |
| Chat AI | `POST /ai/chat`, `POST /ai/chat/stream`, `/ai/chat/sessions`, `GET /ai/quota` |
| Upload AI | `POST /ai/upload/analyze`, `POST /ai/upload/confirm`, `DELETE /ai/upload/:id` |
| Super Admin | `/admin/settings/ai`, `/admin/settings/rerank` dan endpoint kuotanya |

Metode dan route lengkap: [`cmd/api/main.go`](cmd/api/main.go). Validasi role tambahan tetap berlaku pada handler; tidak semua pengguna terautentikasi boleh mengubah master data.

## Tes dan build

Dengan dependency tersedia:

```bash
go test ./...
go vet ./...
go build -o bin/panda-api ./cmd/api
```

Tes regresi berikut hanya memakai standard library dan dapat dijalankan tanpa dependency pihak ketiga:

```bash
go test cmd/api/authorization_test.go -v
go test internal/handler/version.go internal/handler/version_test.go -v
go test internal/storage/key.go internal/storage/key_test.go -v
```

Tes route di `cmd/api/authorization_test.go` memeriksa struktur source/AST; itu bukan pengganti tes HTTP dan database end-to-end. Tes validator menjalankan fungsi validasi yang digunakan aplikasi.

Tes PostgreSQL sungguhan bersifat opt-in:

```bash
go test -tags integration ./internal/service -run Integration -v
```

Mode integration membuat database unik `panda_ragit_<uuid>` lalu menghapusnya setelah selesai; akun test memerlukan hak `CREATEDB`. Jalankan pada PostgreSQL dev/test terisolasi. Alternatif `PANDA_IT_DB_NAME` harus menunjuk database kosong khusus test, tidak pernah database dev/produksi. Tes menerapkan migrasi, seed, dan menulis data pada database tersebut.

Evaluasi kualitas retrieval: baca [`eval/README.md`](eval/README.md).

## Struktur repo

```text
cmd/api/             Entrypoint HTTP dan registrasi route
cmd/rag-eval/        CLI evaluasi retrieval
internal/handler/   HTTP, otorisasi, upload, dan chat
internal/service/   AI/RAG, scope data, dan konfigurasi
internal/storage/   File lokal dan MinIO
internal/database/  Koneksi, migrasi SQL, dan seeding
internal/models/    Model data
```

## Troubleshooting

| Gejala | Pemeriksaan |
| --- | --- |
| `Invalid JWT configuration` | Isi secret acak minimal 32 karakter; hapus placeholder. |
| `DB_PASSWORD belum diisi` / koneksi PostgreSQL gagal | Periksa `.env`, `docker compose ps`, working directory, user/database, dan port `5432`. |
| `module lookup disabled by GOPROXY=off` | Modul belum ada di cache; izinkan akses registry lalu jalankan `go mod download`. |
| Dokumen `401` / `404` | Periksa Bearer token, masa berlaku, serta scope tim/branch. URL dokumen bukan tautan publik. |
| Peringatan MinIO | Lengkapi kredensial dan cek bucket/TLS/jaringan; kosongkan kredensial hanya bila memang memilih storage lokal. |
| Upload versi ditolak | Label versi maksimal 50 karakter, tanpa pemisah path atau karakter khusus nama file. |

## Sebelum produksi

Cabut bypass login dan penyimpanan password PoC, audit dependency, gunakan HTTPS serta pembatasan akses jaringan, tinjau CORS dan TLS PostgreSQL, dan verifikasi isolasi tim/branch dengan tes end-to-end. Jangan memakai role superuser Docker dev pada produksi.
