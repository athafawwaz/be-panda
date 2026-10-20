package service

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/ledongthuc/pdf"
)

// maxExtractedChars membatasi panjang isi yang diindeks per dokumen agar jumlah chunk tetap wajar.
const maxExtractedChars = 400_000

// minPageTextChars: halaman bergambar dengan teks kurang dari ini dianggap hasil scan (tidak terbaca).
const minPageTextChars = 20

// ErrUnsupportedFormat: format file belum bisa dibaca isinya (mis. .doc lama, gambar).
var ErrUnsupportedFormat = errors.New("format file belum didukung untuk pembacaan isi")

// PageText adalah isi satu unit dokumen: halaman PDF, slide PPTX, atau sheet XLSX (DOCX/TXT = satu unit).
type PageText struct {
	Number int
	Text   string
}

// ExtractResult adalah isi dokumen per halaman beserta catatan pembacaannya.
type ExtractResult struct {
	Pages      []PageText
	Unit       string // "hal." | "slide" | "sheet" | "" (tidak berhalaman: DOCX/TXT)
	TotalPages int    // jumlah halaman di file (0 bila tidak berhalaman), termasuk yang tidak terbaca
	EmptyPages []int  // halaman bergambar tanpa lapisan teks, kemungkinan hasil scan
	Truncated  bool   // isi dipotong di maxExtractedChars
}

// FullText menggabungkan isi seluruh halaman yang berteks.
func (r ExtractResult) FullText() string {
	parts := make([]string, 0, len(r.Pages))
	for _, p := range r.Pages {
		if p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// ExtractDocumentText membaca isi teks dari file PDF, DOCX, XLSX, PPTX, atau teks biasa.
// PDF hasil scan (berisi gambar saja) tidak punya lapisan teks sehingga hasilnya kosong.
func ExtractDocumentText(data []byte, ext string) (string, error) {
	res, err := ExtractDocument(data, ext)
	return res.FullText(), err
}

// ExtractDocument membaca isi file per halaman dan mencatat halaman yang tidak terbaca serta pemotongan isi.
func ExtractDocument(data []byte, ext string) (res ExtractResult, err error) {
	if len(data) == 0 {
		return ExtractResult{}, nil
	}
	// Parser PDF/XML bisa panic pada file rusak; jangan sampai menggagalkan proses upload
	defer func() {
		if r := recover(); r != nil {
			res, err = ExtractResult{}, fmt.Errorf("file tidak dapat dibaca: %v", r)
		}
	}()

	var raw rawPages
	unit := ""
	switch strings.ToLower(ext) {
	case ".pdf":
		raw, err = extractPDFPages(data)
		unit = "hal."
	case ".docx":
		raw, err = extractZipXMLPages(data, func(name string) bool { return name == "word/document.xml" })
	case ".pptx":
		raw, err = extractZipXMLPages(data, func(name string) bool {
			return strings.HasPrefix(name, "ppt/slides/slide") && strings.HasSuffix(name, ".xml")
		})
		unit = "slide"
	case ".xlsx":
		raw, err = extractXLSXPages(data)
		unit = "sheet"
	case ".txt", ".csv", ".md", ".log", ".json":
		raw = rawPages{texts: []string{string(data)}, total: 1}
	default:
		return ExtractResult{}, ErrUnsupportedFormat
	}
	return buildExtractResult(raw, unit), err
}

// rawPages adalah teks mentah per halaman sebelum normalisasi.
type rawPages struct {
	texts      []string
	total      int          // jumlah halaman di file; bisa lebih dari len(texts) bila pembacaan berhenti di batas ukuran
	imagePages map[int]bool // nomor halaman (1-based) yang memuat gambar
}

func buildExtractResult(raw rawPages, unit string) ExtractResult {
	res := ExtractResult{Unit: unit, Truncated: len(raw.texts) < raw.total}
	if unit != "" {
		res.TotalPages = raw.total
	}
	used := 0
	for i, t := range raw.texts {
		text := normalizeExtractedText(t)
		if used+len(text) > maxExtractedChars {
			text = cutUTF8(text, maxExtractedChars-used)
			res.Truncated = true
		}
		if raw.imagePages[i+1] && nonSpaceLen(text) < minPageTextChars {
			res.EmptyPages = append(res.EmptyPages, i+1)
		}
		res.Pages = append(res.Pages, PageText{Number: i + 1, Text: text})
		used += len(text)
		if used >= maxExtractedChars {
			res.Truncated = res.Truncated || i < len(raw.texts)-1
			break
		}
	}
	return res
}

func nonSpaceLen(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// cutUTF8 memotong s menjadi maksimal max byte tanpa memecah karakter multi-byte.
func cutUTF8(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

var dotLeaders = regexp.MustCompile(`(?:\. ?){4,}`)

func normalizeExtractedText(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.NewReplacer(
		"\r\n", "\n", "\r", "\n", "\x00", "", "\u00a0", " ", "\ufffd", "",
		// Ligatur PDF ("Veri\ufb01kasi") dipecah agar cocok dengan kata kunci pencarian
		"\ufb00", "ff", "\ufb01", "fi", "\ufb02", "fl", "\ufb03", "ffi", "\ufb04", "ffl",
	).Replace(s)

	// Titik-titik penuntun daftar isi ("Pendahuluan ........ 3") hanya menambah noise pencarian
	s = dotLeaders.ReplaceAllString(s, " … ")

	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		ln = strings.Join(strings.Fields(ln), " ")
		if ln == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// ─── PDF ─────────────────────────────────────────────────────────────────────

func extractPDFPages(data []byte) (rawPages, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return rawPages{}, err
	}
	raw := rawPages{total: r.NumPage(), imagePages: map[int]bool{}}
	size := 0
	for i := 1; i <= raw.total; i++ {
		p := r.Page(i)
		text := ""
		if !p.V.IsNull() {
			text = pdfPageText(p)
			raw.imagePages[i] = pageHasImage(p)
		}
		raw.texts = append(raw.texts, text)
		if size += len(text); size > maxExtractedChars {
			break
		}
	}
	return raw, nil
}

// pageHasImage: halaman memuat gambar (XObject Image/Form), ciri halaman hasil scan bila tanpa teks.
// Halaman kosong sungguhan (tanpa gambar) tidak dihitung sebagai halaman yang tidak terbaca.
func pageHasImage(p pdf.Page) (found bool) {
	defer func() {
		if recover() != nil {
			found = false
		}
	}()
	xobjects := p.Resources().Key("XObject")
	for _, name := range xobjects.Keys() {
		switch xobjects.Key(name).Key("Subtype").Name() {
		case "Image", "Form":
			return true
		}
	}
	return false
}

// pdfPageText menyusun ulang teks per baris berdasarkan posisi karakter,
// sehingga spasi antar kata & pergantian baris tetap terbaca.
func pdfPageText(p pdf.Page) (text string) {
	defer func() {
		if recover() != nil {
			text, _ = p.GetPlainText(nil)
		}
	}()

	type streamText struct {
		pdf.Text
		order int // urutan di content stream
	}
	var chars []streamText
	for i, ch := range p.Content().Text {
		// Library menyisipkan "\n" di akhir setiap objek teks; pergantian baris ditentukan dari posisi Y
		if ch.S != " " && strings.TrimSpace(ch.S) == "" {
			continue
		}
		chars = append(chars, streamText{ch, i})
	}
	if len(chars) == 0 {
		plain, _ := p.GetPlainText(nil)
		return plain
	}

	// Kelompokkan per baris dari atas ke bawah
	sort.SliceStable(chars, func(i, j int) bool { return chars[i].Y > chars[j].Y })
	var lines [][]streamText
	for _, ch := range chars {
		n := len(lines)
		if n > 0 {
			first := lines[n-1][0]
			if math.Abs(first.Y-ch.Y) <= math.Max(2, first.FontSize*0.4) {
				lines[n-1] = append(lines[n-1], ch)
				continue
			}
		}
		lines = append(lines, []streamText{ch})
	}

	var sb strings.Builder
	for _, line := range lines {
		// Dalam satu baris, urutan stream lebih andal daripada urutan X: font CID sering memberi
		// posisi X yang tidak akurat sehingga pengurutan X mengacak huruf.
		sort.SliceStable(line, func(i, j int) bool { return line[i].order < line[j].order })
		// Font CID (mis. PDF dari Chrome/Edge) tidak memberi lebar huruf (W=0): posisi X hanya berubah
		// antar potongan teks, jadi akhir potongan sebelumnya diperkirakan dari lebar huruf standar.
		runEnd := 0.0
		for k, ch := range line {
			if k > 0 {
				prev := line[k-1]
				end := prev.X + prev.W
				if prev.W == 0 {
					end = runEnd
				}
				gapAfter := ch.X-end > prev.FontSize*0.15
				jumpBack := ch.X < prev.X-prev.FontSize // potongan teks baru di sebelah kiri pada baris yang sama
				if (gapAfter || jumpBack) && prev.S != " " && ch.S != " " {
					sb.WriteByte(' ')
				}
			}
			if ch.W > 0 {
				runEnd = ch.X + ch.W
			} else {
				if k == 0 || ch.X != line[k-1].X {
					runEnd = ch.X
				}
				runEnd += estimateTextWidth(ch.Text)
			}
			sb.WriteString(ch.S)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// helveticaWidths: lebar karakter ASCII 32–126 font Helvetica/Arial (per 1000 em).
var helveticaWidths = [95]int{
	278, 278, 355, 556, 556, 889, 667, 191, 333, 333, 389, 584, 278, 333, 278, 278, // spasi – /
	556, 556, 556, 556, 556, 556, 556, 556, 556, 556, // 0 – 9
	278, 278, 584, 584, 584, 556, 1015, // : – @
	667, 667, 722, 722, 667, 611, 778, 722, 278, 500, 667, 556, 833, // A – M
	722, 778, 667, 778, 722, 667, 611, 722, 667, 944, 667, 667, 611, // N – Z
	278, 278, 278, 469, 556, 333, // [ – `
	556, 556, 500, 556, 556, 278, 556, 556, 222, 222, 500, 222, 833, // a – m
	556, 556, 556, 556, 333, 500, 278, 556, 500, 722, 500, 500, 500, // n – z
	334, 260, 334, 584, // { – ~
}

// estimateTextWidth memperkirakan lebar teks bila PDF tidak menyediakan lebar huruf.
func estimateTextWidth(ch pdf.Text) float64 {
	font := strings.ToLower(ch.Font)
	if strings.Contains(font, "courier") || strings.Contains(font, "mono") || strings.Contains(font, "consola") {
		return 0.6 * ch.FontSize * float64(len([]rune(ch.S)))
	}
	scale := 1.0
	if strings.Contains(font, "verdana") {
		scale = 1.18 // Verdana jauh lebih lebar dari Arial
	}
	if strings.Contains(font, "bold") || strings.Contains(font, "black") || strings.Contains(font, "heavy") {
		scale *= 1.06
	}
	total := 0
	for _, r := range ch.S {
		if r >= 32 && r <= 126 {
			total += helveticaWidths[r-32]
		} else {
			total += 556
		}
	}
	return float64(total) / 1000 * ch.FontSize * scale
}

// ─── DOCX / PPTX ─────────────────────────────────────────────────────────────

var trailingNumber = regexp.MustCompile(`(\d+)\.xml$`)

func xmlPartNumber(name string) int {
	if m := trailingNumber.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// extractZipXMLPages membaca bagian XML Office yang dipilih; satu bagian = satu halaman (slide PPTX).
func extractZipXMLPages(data []byte, want func(name string) bool) (rawPages, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return rawPages{}, err
	}
	var parts []*zip.File
	for _, f := range zr.File {
		if want(f.Name) {
			parts = append(parts, f)
		}
	}
	// slide2.xml harus sebelum slide10.xml
	sort.Slice(parts, func(i, j int) bool { return xmlPartNumber(parts[i].Name) < xmlPartNumber(parts[j].Name) })

	raw := rawPages{total: len(parts)}
	size := 0
	for _, f := range parts {
		txt := ""
		if rc, err := f.Open(); err == nil {
			txt, _ = officeXMLText(rc)
			rc.Close()
		}
		raw.texts = append(raw.texts, txt)
		if size += len(txt); size > maxExtractedChars {
			break
		}
	}
	return raw, nil
}

// officeXMLText mengambil isi elemen teks (<w:t>, <a:t>) dari XML Office Open,
// dengan baris baru per paragraf dan pemisah " | " antar sel tabel.
func officeXMLText(r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)
	var sb strings.Builder
	inText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return sb.String(), nil
		}
		if err != nil {
			return sb.String(), err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				sb.WriteByte('\t')
			case "br", "cr":
				sb.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				sb.WriteByte('\n')
			case "tc":
				sb.WriteString(" | ")
			}
		case xml.CharData:
			if inText {
				sb.Write(t)
			}
		}
	}
}

// ─── XLSX ────────────────────────────────────────────────────────────────────

// extractXLSXPages membaca setiap sheet sebagai satu halaman, baris ditulis "sel1 | sel2 | ...".
func extractXLSXPages(data []byte) (rawPages, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return rawPages{}, err
	}

	var shared []string
	var sheets []*zip.File
	for _, f := range zr.File {
		switch {
		case f.Name == "xl/sharedStrings.xml":
			if rc, err := f.Open(); err == nil {
				shared = readSharedStrings(rc)
				rc.Close()
			}
		case strings.HasPrefix(f.Name, "xl/worksheets/sheet") && strings.HasSuffix(f.Name, ".xml"):
			sheets = append(sheets, f)
		}
	}
	sort.Slice(sheets, func(i, j int) bool { return xmlPartNumber(sheets[i].Name) < xmlPartNumber(sheets[j].Name) })

	raw := rawPages{total: len(sheets)}
	size := 0
	for _, f := range sheets {
		var sb strings.Builder
		if rc, err := f.Open(); err == nil {
			sb.WriteString(fmt.Sprintf("Sheet %d:\n", xmlPartNumber(f.Name)))
			writeSheetRows(&sb, rc, shared)
			rc.Close()
		}
		raw.texts = append(raw.texts, sb.String())
		if size += sb.Len(); size > maxExtractedChars {
			break
		}
	}
	return raw, nil
}

func readSharedStrings(r io.Reader) []string {
	dec := xml.NewDecoder(r)
	var out []string
	var cur strings.Builder
	inSI, inText := false, false
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "si" {
				inSI = true
				cur.Reset()
			} else if t.Name.Local == "t" && inSI {
				inText = true
			}
		case xml.EndElement:
			if t.Name.Local == "si" {
				inSI = false
				out = append(out, cur.String())
			} else if t.Name.Local == "t" {
				inText = false
			}
		case xml.CharData:
			if inText {
				cur.Write(t)
			}
		}
	}
}

// writeSheetRows menulis setiap baris sheet sebagai "sel1 | sel2 | ...".
func writeSheetRows(sb *strings.Builder, r io.Reader, shared []string) {
	dec := xml.NewDecoder(r)
	var row []string
	var cellType string
	var val strings.Builder
	inValue := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				row = row[:0]
			case "c":
				cellType = ""
				for _, a := range t.Attr {
					if a.Name.Local == "t" {
						cellType = a.Value
					}
				}
				val.Reset()
			case "v", "t":
				inValue = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v", "t":
				inValue = false
			case "c":
				v := strings.TrimSpace(val.String())
				if cellType == "s" {
					if idx, err := strconv.Atoi(v); err == nil && idx >= 0 && idx < len(shared) {
						v = shared[idx]
					}
				}
				if v != "" {
					row = append(row, v)
				}
			case "row":
				if len(row) > 0 {
					sb.WriteString(strings.Join(row, " | "))
					sb.WriteByte('\n')
				}
			}
		case xml.CharData:
			if inValue {
				val.Write(t)
			}
		}
	}
}
