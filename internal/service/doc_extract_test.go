package service

import (
	"archive/zip"
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

var updateGolden = flag.Bool("update", false, "tulis ulang file golden di testdata/golden (go test ./internal/service -run Golden -update)")

// ─── Pembuat fixture (tanpa file biner di repo) ─────────────────────────────

type testPDFPage struct {
	Lines []string // teks per baris, font Helvetica 12 pt
	Image bool     // tempel gambar, meniru halaman hasil scan bila tanpa teks
}

// buildTestPDF membuat PDF minimal yang valid (xref benar) agar parser PDF diuji dengan file nyata.
func buildTestPDF(pages []testPDFPage) []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"", // diisi setelah nomor objek halaman diketahui
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		"<< /Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace /DeviceGray /BitsPerComponent 8 /Length 1 >>\nstream\n\xff\nendstream",
	}
	var kids []string
	for _, p := range pages {
		pageNum := len(objs) + 1
		kids = append(kids, fmt.Sprintf("%d 0 R", pageNum))
		var content strings.Builder
		if p.Image {
			content.WriteString("q 200 0 0 200 100 300 cm /Im1 Do Q\n")
		}
		if len(p.Lines) > 0 {
			content.WriteString("BT /F1 12 Tf 14 TL 72 720 Td\n")
			for _, l := range p.Lines {
				l = strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`).Replace(l)
				content.WriteString("(" + l + ") Tj T*\n")
			}
			content.WriteString("ET\n")
		}
		resources := "<< /Font << /F1 3 0 R >> >>"
		if p.Image {
			resources = "<< /Font << /F1 3 0 R >> /XObject << /Im1 4 0 R >> >>"
		}
		objs = append(objs,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources %s /Contents %d 0 R >>", resources, pageNum+1),
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", content.Len(), content.String()))
	}
	objs[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages))

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return buf.Bytes()
}

func buildTestZip(files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(content))
	}
	_ = zw.Close()
	return buf.Bytes()
}

const wordNS = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`

func wordParagraph(text string) string {
	return "<w:p><w:r><w:t>" + text + "</w:t></w:r></w:p>"
}

func testDOCX() []byte {
	body := wordParagraph("BERITA ACARA SERAH TERIMA") +
		wordParagraph("Aplikasi CorePay versi 1.1 diserahterimakan pada 12 Oktober 2026.") +
		"<w:tbl><w:tr><w:tc>" + wordParagraph("Modul") + "</w:tc><w:tc>" + wordParagraph("Status") + "</w:tc></w:tr>" +
		"<w:tr><w:tc>" + wordParagraph("Pembayaran") + "</w:tc><w:tc>" + wordParagraph("Lulus UAT") + "</w:tc></w:tr></w:tbl>"
	return buildTestZip(map[string]string{
		"word/document.xml": `<w:document ` + wordNS + `><w:body>` + body + `</w:body></w:document>`,
	})
}

func pptxSlide(text string) string {
	return `<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">` +
		`<p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>` + text + `</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`
}

func testPPTX() []byte {
	return buildTestZip(map[string]string{
		"ppt/slides/slide1.xml":  pptxSlide("Rencana Uji Penetrasi 2026"),
		"ppt/slides/slide2.xml":  pptxSlide("Ruang lingkup: portal pelanggan"),
		"ppt/slides/slide10.xml": pptxSlide("Penutup"),
	})
}

func testXLSX() []byte {
	ns := `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"`
	return buildTestZip(map[string]string{
		"xl/sharedStrings.xml": `<sst ` + ns + `><si><t>Skenario</t></si><si><t>Hasil</t></si><si><t>Login SSO</t></si><si><t>Lulus</t></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet ` + ns + `><sheetData>` +
			`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>` +
			`<row r="2"><c r="A2" t="s"><v>2</v></c><c r="B2" t="s"><v>3</v></c><c r="C2"><v>42</v></c></row>` +
			`</sheetData></worksheet>`,
	})
}

func testMixedPDF() []byte {
	return buildTestPDF([]testPDFPage{
		{Lines: []string{"BERITA ACARA SERAH TERIMA", "Aplikasi CorePay versi 1.1 telah diuji dan dinyatakan lulus."}},
		{Image: true}, // halaman tanda tangan basah hasil scan
		{Lines: []string{"Lampiran: daftar modul yang diserahterimakan."}},
		{}, // halaman kosong sungguhan, bukan hasil scan
	})
}

// ─── Ekstraksi ───────────────────────────────────────────────────────────────

func TestExtractPDFDetectsScannedPages(t *testing.T) {
	res, err := ExtractDocument(testMixedPDF(), ".pdf")
	if err != nil {
		t.Fatalf("PDF valid gagal dibaca: %v", err)
	}
	if res.Unit != "hal." || res.TotalPages != 4 {
		t.Fatalf("unit/jumlah halaman salah: %q %d", res.Unit, res.TotalPages)
	}
	if !reflect.DeepEqual(res.EmptyPages, []int{2}) {
		t.Fatalf("hanya halaman bergambar tanpa teks yang dihitung tidak terbaca, dapat %v", res.EmptyPages)
	}
	full := res.FullText()
	if !strings.Contains(full, "dinyatakan lulus") || !strings.Contains(full, "Lampiran") {
		t.Fatalf("teks halaman berteks harus terbaca, dapat:\n%s", full)
	}
	if res.Truncated {
		t.Fatalf("dokumen pendek tidak boleh ditandai terpotong")
	}
}

func TestExtractDocumentTextMatchesFullText(t *testing.T) {
	data := testMixedPDF()
	text, err := ExtractDocumentText(data, ".PDF")
	res, _ := ExtractDocument(data, ".pdf")
	if err != nil || text != res.FullText() {
		t.Fatalf("ExtractDocumentText harus sama dengan FullText: err=%v", err)
	}
}

func TestExtractOfficeFormats(t *testing.T) {
	docx, err := ExtractDocument(testDOCX(), ".docx")
	if err != nil || docx.Unit != "" || docx.TotalPages != 0 || !strings.Contains(docx.FullText(), "CorePay versi 1.1") {
		t.Fatalf("DOCX salah dibaca: err=%v %+v", err, docx)
	}

	pptx, err := ExtractDocument(testPPTX(), ".pptx")
	if err != nil || pptx.Unit != "slide" || pptx.TotalPages != 3 {
		t.Fatalf("PPTX salah dibaca: err=%v %+v", err, pptx)
	}
	if pptx.Pages[1].Text != "Ruang lingkup: portal pelanggan" || pptx.Pages[2].Text != "Penutup" {
		t.Fatalf("slide2 harus sebelum slide10: %+v", pptx.Pages)
	}

	xlsx, err := ExtractDocument(testXLSX(), ".xlsx")
	if err != nil || xlsx.Unit != "sheet" || !strings.Contains(xlsx.FullText(), "Login SSO | Lulus | 42") {
		t.Fatalf("XLSX salah dibaca: err=%v %q", err, xlsx.FullText())
	}
}

func TestExtractUnsupportedAndEmpty(t *testing.T) {
	if _, err := ExtractDocument([]byte("x"), ".doc"); err != ErrUnsupportedFormat {
		t.Fatalf(".doc lama harus ErrUnsupportedFormat, dapat %v", err)
	}
	if res, err := ExtractDocument(nil, ".pdf"); err != nil || res.FullText() != "" {
		t.Fatalf("file kosong harus menghasilkan teks kosong tanpa error")
	}
	if _, err := ExtractDocument([]byte("bukan pdf"), ".pdf"); err == nil {
		t.Fatalf("PDF rusak harus menghasilkan error, bukan panic")
	}
}

func TestExtractTruncatesLongDocumentSafely(t *testing.T) {
	line := strings.Repeat("é", 50) + "\n" // karakter 2 byte agar pemotongan diuji di batas rune
	data := []byte(strings.Repeat(line, maxExtractedChars/len(line)+50))
	res, err := ExtractDocument(data, ".txt")
	if err != nil {
		t.Fatal(err)
	}
	full := res.FullText()
	if !res.Truncated || len(full) > maxExtractedChars {
		t.Fatalf("dokumen panjang harus terpotong ≤ %d byte, dapat truncated=%v len=%d", maxExtractedChars, res.Truncated, len(full))
	}
	if !utf8.ValidString(full) {
		t.Fatalf("pemotongan memecah karakter multi-byte")
	}
}

func TestNormalizeExtractedText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"ligatur", "Veriﬁkasi ﬂow", "Verifikasi flow"},
		{"dot leader daftar isi", "Pendahuluan ........ 3", "Pendahuluan … 3"},
		{"spasi & baris kosong", "a   b\r\n\r\n\r\n\r\nc d", "a b\n\nc d"},
		{"karakter rusak", "da\x00ta�", "data"},
	}
	for _, c := range cases {
		if got := normalizeExtractedText(c.in); got != c.want {
			t.Errorf("%s: dapat %q, seharusnya %q", c.name, got, c.want)
		}
	}
}

func TestCutUTF8(t *testing.T) {
	if got := cutUTF8("aé", 2); got != "a" {
		t.Fatalf("tidak boleh memotong di tengah karakter, dapat %q", got)
	}
	if got := cutUTF8("abc", 0); got != "" {
		t.Fatalf("batas 0 harus kosong, dapat %q", got)
	}
}

// ─── Golden file ─────────────────────────────────────────────────────────────

// TestExtractGolden membandingkan hasil ekstraksi fixture dengan testdata/golden. Perubahan ekstraksi yang disengaja
// diperbarui dengan -update lalu diperiksa diff-nya sebelum di-commit.
func TestExtractGolden(t *testing.T) {
	fixtures := []struct {
		name, ext string
		data      []byte
	}{
		{"mixed_scanned_pdf", ".pdf", testMixedPDF()},
		{"bast_docx", ".docx", testDOCX()},
		{"pentest_pptx", ".pptx", testPPTX()},
		{"uat_xlsx", ".xlsx", testXLSX()},
	}
	for _, f := range fixtures {
		res, err := ExtractDocument(f.data, f.ext)
		if err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "unit=%q total_pages=%d empty_pages=%v truncated=%v\n", res.Unit, res.TotalPages, res.EmptyPages, res.Truncated)
		for _, p := range res.Pages {
			fmt.Fprintf(&sb, "--- %d ---\n%s\n", p.Number, p.Text)
		}
		got := sb.String()

		path := filepath.Join("testdata", "golden", f.name+".golden.txt")
		if *updateGolden {
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(got), 0644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: golden belum ada, jalankan dengan -update: %v", f.name, err)
		}
		if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
			t.Errorf("%s: hasil ekstraksi berubah.\n--- dapat ---\n%s\n--- golden ---\n%s", f.name, got, want)
		}
	}
}
