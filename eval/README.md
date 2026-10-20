# Evaluasi Retrieval Agentic AI (`cmd/rag-eval`)

Alat ini mengukur seberapa sering Agentic AI menemukan dokumen dan potongan isi yang benar. Setiap perubahan pada ekstraksi, chunking, atau pencarian wajib membandingkan hasilnya dengan baseline sebelum di-merge (`docs/RAG_IMPROVEMENT_PLAN.md` R6 dan §6).

Retrieval dijalankan dengan engine builtin, tanpa memanggil LLM. Hasilnya deterministik dan tidak memakai kuota AI.

## 1. Siapkan pertanyaan uji

1. Salin `rag_eval.example.json` menjadi `rag_eval.json`.
2. Isi 30–50 pertanyaan positif dan 5–10 pertanyaan negatif berdasarkan dokumen yang ada di database dev. Pertanyaan disusun oleh tim Tata Kelola.
3. Campur pertanyaan dari beberapa tim dan jenis dokumen (PRD, UAT, BAST, SOP, …).
4. Setiap kali ada laporan jawaban AI yang salah, tambahkan pertanyaannya sebagai kasus uji baru.

| Field | Wajib | Isi |
| :-- | :-: | :-- |
| `id` | ✓ | Kode unik, mis. `q01`, `n01` |
| `question` | ✓ | Pertanyaan seperti yang akan diketik user |
| `expect_docs` | positif | `standardized_name` dokumen yang benar (lihat kartu dokumen atau kolom `documents.standardized_name`). Boleh lebih dari satu. |
| `expect_phrases` | opsional | Frasa yang harus muncul di potongan isi yang dikirim ke AI. Bila diisi, Hit@3 ikut dinilai. |
| `history` | opsional | Pertanyaan sebelumnya di percakapan yang sama (untuk menguji pertanyaan lanjutan). Dijalankan berurutan. |
| `negative` | opsional | `true` bila topiknya memang tidak ada di dokumen mana pun. Pencarian yang benar tidak mengembalikan dokumen, sehingga AI menjawab tanpa mengutip dokumen (label *Pengetahuan umum*; tanpa LLM: "tidak ditemukan"). |
| `filter_module` | opsional | `APPLICATION` atau `EVIDENCE` bila pertanyaan diajukan dengan filter modul |

## 2. Jalankan

Dari folder `backend`, setelah backend versi terbaru pernah dijalankan sekali (agar migrasi diterapkan):

```bash
# Ukur baseline
go run ./cmd/rag-eval -reindex -out eval/baseline.json

# Setelah perubahan: bandingkan dengan baseline
go run ./cmd/rag-eval -out eval/hasil.json -baseline eval/baseline.json
```

- **`-reindex`:** indeks ulang dokumen yang belum memakai versi indeks terbaru. Tanpa flag ini, alat memakai jalur pencarian lama dan memberi peringatan.
- **`-badge 2419901`:** jalankan sebagai user tertentu untuk menguji isolasi tim. Defaultnya Super Admin, yang melihat semua tim.
- **Gerbang baseline:** dengan `-baseline`, program keluar dengan kode 1 bila Recall@4 atau akurasi Document Guard turun.

## 3. Metrik

| Metrik | Arti | Target awal |
| :-- | :-- | :-- |
| Recall@4 | Pertanyaan positif yang dokumen benarnya masuk 4 dokumen teratas | ≥ 0,90 |
| Hit@3 | Pertanyaan dengan `expect_phrases` yang frasanya ada di ≤3 potongan konteks dokumen yang benar | ≥ 0,75 |
| MRR | Rata-rata 1/peringkat dokumen benar pertama | Naik tiap fase |
| Guard | Pertanyaan negatif yang pencariannya tidak mengembalikan dokumen | ≥ 0,90 |
| p95 | Waktu retrieval persentil ke-95, tanpa LLM | ≤ 800 ms pada 5.000 chunk |

## 4. Perbandingan reranker (`-rerank`)

```bash
go run ./cmd/rag-eval -rerank -out eval/hasil-rerank.json
```

Untuk setiap pertanyaan positif, kandidat pipeline tool `search_documents` yang sama dinilai dua kali: heuristik saja vs reranker model yang aktif di **Docs → Reranker**. Laporan menampilkan peringkat dokumen benar, MRR, Hit@3 (dokumen benar di 3 teratas), nDCG@5, dan latensi reranker. Mode ini memanggil API reranker sehingga memakai kuotanya, dan mengikuti saklar *isi dokumen boleh dikirim* (mati = hanya judul & ringkasan yang dikirim).

Perilaku chat (sapaan, pertanyaan kemampuan, label jawaban, tool calling) diuji lewat `go test ./internal/service` dengan server LLM tiruan, termasuk format tool call Gemini 3 (`thought_signature`). Uji manual terakhir memakai `gemini-3.6-flash` lewat endpoint OpenAI-compatible Google.

`rag_eval.json` dan laporan hasil mungkin memuat judul dokumen internal, jadi jangan dibagikan ke luar tim.
