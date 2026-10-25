// База знаний аналитики Альфа-Банка (Приложение 2 кейса) и RAG-поиск по ней.
//
// Материалы кладутся в data/alfa_analytics/ в форматах .md, .txt, .pdf (через
// утилиту pdftotext из poppler-utils) или .docx. Каждый документ режется на
// фрагменты (chunks); у фрагмента сохраняется документ-источник, чтобы любой вывод
// «позиции банка» можно было проверить по первоисточнику.
// Материалы кейса лежат в data/alfa_analytics (PDF-сканы) и data/alfa_analytics/_text (их расшифровки).
package main

import (
	"archive/zip"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

type AnalyticsDoc struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Date      string `json:"date"`
	File      string `json:"file"`
	Author    string `json:"author,omitempty"`
	Synthetic bool   `json:"synthetic"`
	// TextSource — откуда взят текст: pdf (текстовый слой), transcript (PDF-скан → расшифровка
	// в _text/), vision (страницы расшифрованы Qwen3-VL), md/txt/docx.
	TextSource string `json:"text_source"`
	Pages      int    `json:"pages,omitempty"`
	// Stale — материал старше порога актуальности (ANALYTICS_STALE_DAYS, по умолчанию 180 дней).
	// Такие материалы не удаляются, но в выводах помечаются и ранжируются ниже.
	Stale   bool `json:"stale"`
	AgeDays int  `json:"age_days"`
}

type Chunk struct {
	ID        string   `json:"id"`
	DocID     string   `json:"doc_id"`
	DocTitle  string   `json:"doc_title"`
	Date      string   `json:"date"`
	File      string   `json:"file,omitempty"`
	Page      int      `json:"page,omitempty"`
	Text      string   `json:"text"`
	Factors   []string `json:"factors"`
	Sectors   []string `json:"sectors"`
	Synthetic bool     `json:"synthetic"`
	Stale     bool     `json:"stale,omitempty"`
	Table     bool     `json:"table,omitempty"` // фрагмент — таблица чисел (ниже в поиске позиции банка)
}

type ChunkHit struct {
	Chunk
	Score float64 `json:"score"`
}

type KnowledgeBase struct {
	Docs      []AnalyticsDoc
	Chunks    []Chunk
	Forecasts []Forecast // прогнозы банка, извлечённые из текстов (forecasts.go)
	vec       VectorizerBackend
	mu        sync.Mutex
	vecs      []Vector
}

var mdTitleRe = regexp.MustCompile(`(?m)^#\s+(.+)$`)
var metaDateRe = regexp.MustCompile(`(?mi)^(дата|date):\s*(.+)$`)
var metaAuthorRe = regexp.MustCompile(`(?mi)^(автор|author):\s*(.+)$`)
var metaFileRe = regexp.MustCompile(`(?mi)^(файл|file):\s*(.+)$`)
var pageMarkRe = regexp.MustCompile(`<!--\s*page\s+(\d+)\s*-->`)
var paraSplitRe = regexp.MustCompile(`\n\s*\n`)

// Служебный текст, который есть почти на каждой странице PDF банка и не несёт позиции банка.
var boilerplateTerms = []string{"все права защищены", "несанкционированное копирование", "генеральная лицензия цб",
	"не является индивидуальной инвестиционной рекомендацией", "аффилированные лица ведут", "контакты и дисклеймер"}

func readDocument(path string) (string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".txt":
		b, err := os.ReadFile(path)
		return string(b), err
	case ".pdf":
		out, err := exec.Command("pdftotext", "-layout", "-enc", "UTF-8", path, "-").Output()
		if err != nil {
			return "", fmt.Errorf("pdftotext недоступен или не смог прочитать %s (установите poppler-utils): %w", path, err)
		}
		// pdftotext разделяет страницы символом \f — превращаем в метки страниц для ссылок «стр. N».
		pages := strings.Split(string(out), "\f")
		if cfg.VisionOCR && cfg.LLMEnabled() {
			pages = enrichPDFWithVision(path, pages)
		}
		var b strings.Builder
		for i, pg := range pages {
			if strings.TrimSpace(pg) == "" {
				continue
			}
			fmt.Fprintf(&b, "\n<!-- page %d -->\n%s\n", i+1, pg)
		}
		return b.String(), nil
	case ".docx":
		zr, err := zip.OpenReader(path)
		if err != nil {
			return "", err
		}
		defer zr.Close()
		for _, f := range zr.File {
			if f.Name == "word/document.xml" {
				rc, err := f.Open()
				if err != nil {
					return "", err
				}
				data, _ := io.ReadAll(rc)
				rc.Close()
				s := strings.ReplaceAll(string(data), "</w:p>", "\n\n")
				return tagRe.ReplaceAllString(s, ""), nil
			}
		}
		return "", fmt.Errorf("%s: не найден word/document.xml", path)
	}
	return "", nil
}

// fileKey — нормализованное имя файла для сопоставления PDF и его расшифровки.
func fileKey(name string) string {
	name = strings.ToLower(strings.TrimSuffix(filepath.Base(name), filepath.Ext(name)))
	name = regexp.MustCompile(`^[0-9a-f]{8}-`).ReplaceAllString(name, "") // префикс загрузки
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// textChars — сколько «содержательных» символов в тексте (без меток страниц и пробелов).
func textChars(s string) int {
	n := 0
	for _, r := range pageMarkRe.ReplaceAllString(s, "") {
		if unicode.IsLetter(r) {
			n++
		}
	}
	return n
}

// LoadKnowledgeBase загружает и режет материалы.
//
// Порядок: файлы из data/alfa_analytics (PDF/DOCX/MD/TXT). Если PDF — скан без текстового слоя
// (так устроены все PDF кейса), используется расшифровка из data/alfa_analytics/_text/<имя>.md
// с теми же метками страниц, а при ANALYTICS_VISION_OCR=true — расшифровка моделью Qwen3-VL.
// Расшифровка без PDF рядом тоже загружается (в репозитории можно хранить только тексты).
func LoadKnowledgeBase(vec VectorizerBackend) (*KnowledgeBase, error) {
	kb := &KnowledgeBase{vec: vec}
	transcripts := map[string]string{} // ключ имени PDF → путь к расшифровке
	if entries, err := os.ReadDir(filepath.Join(cfg.AnalyticsDir, "_text")); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
				continue
			}
			p := filepath.Join(cfg.AnalyticsDir, "_text", e.Name())
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			key := fileKey(e.Name())
			if m := metaFileRe.FindStringSubmatch(string(b)); m != nil {
				key = fileKey(strings.TrimSpace(m[2]))
			}
			transcripts[key] = p
		}
	}
	used := map[string]bool{}
	load := func(dir string, synthetic bool) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.EqualFold(e.Name(), "README.md") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			isPDF := strings.EqualFold(filepath.Ext(path), ".pdf")
			text, err := readDocument(path)
			source := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
			if isPDF && cfg.VisionOCR && cfg.LLMEnabled() {
				source = "vision"
			}
			// Скан без текста: подставляем расшифровку.
			if isPDF && (err != nil || textChars(text) < 300*max(1, strings.Count(text, "<!-- page"))/4) {
				if tp, ok := transcripts[fileKey(e.Name())]; ok {
					b, rerr := os.ReadFile(tp)
					if rerr == nil {
						text, err, source = string(b), nil, "transcript"
						used[tp] = true
					}
				} else if err == nil {
					logWarn("%s: в PDF нет текстового слоя и нет расшифровки в _text/ — включите ANALYTICS_VISION_OCR=true", e.Name())
				}
			}
			if err != nil {
				logWarn("%v", err)
				continue
			}
			if strings.TrimSpace(text) == "" {
				continue
			}
			kb.addDocument(path, text, synthetic, source)
		}
		return nil
	}
	_ = load(cfg.AnalyticsDir, false)
	for _, tp := range sortedKeys(invertMap(transcripts)) {
		if !used[tp] { // расшифровка без PDF рядом
			if b, err := os.ReadFile(tp); err == nil {
				kb.addDocument(tp, string(b), false, "transcript")
			}
		}
	}
	if len(kb.Docs) == 0 {
		logWarn("Материалы Альфа-Банка не найдены в %s — позиция банка в выводах будет недоступна", cfg.AnalyticsDir)
	}
	kb.Forecasts = ExtractForecasts(kb)
	stale := 0
	for _, d := range kb.Docs {
		if d.Stale {
			stale++
		}
	}
	logInfo("Аналитика: %d документов (%d устаревших), %d фрагментов, %d прогнозов", len(kb.Docs), stale, len(kb.Chunks), len(kb.Forecasts))
	return kb, nil
}

func invertMap(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[v] = k
	}
	return out
}

// analyticsNow — «сегодня» для оценки актуальности материалов (можно зафиксировать для демо).
func analyticsNow() time.Time {
	if t := parseDate(os.Getenv("ANALYTICS_NOW")); t != nil {
		return *t
	}
	return time.Now()
}

func (kb *KnowledgeBase) addDocument(path, text string, synthetic bool, source string) {
	docID := fmt.Sprintf("A%d", len(kb.Docs)+1)
	file := filepath.Base(path)
	if m := metaFileRe.FindStringSubmatch(text); m != nil {
		file = strings.TrimSpace(m[2])
	}
	title := strings.TrimSuffix(file, filepath.Ext(file))
	if m := mdTitleRe.FindStringSubmatch(text); m != nil {
		title = strings.TrimSpace(m[1])
	} else {
		for _, line := range strings.Split(pageMarkRe.ReplaceAllString(text, ""), "\n") {
			if l := strings.TrimSpace(line); runeLen(l) > 8 {
				title = shorten(l, 120)
				break
			}
		}
	}
	date, author := "", ""
	if m := metaDateRe.FindStringSubmatch(text); m != nil {
		date = strings.TrimSpace(m[2])
	}
	if m := metaAuthorRe.FindStringSubmatch(text); m != nil {
		author = strings.TrimSpace(m[2])
	}
	doc := AnalyticsDoc{ID: docID, Title: title, Date: date, File: file, Author: author, Synthetic: synthetic,
		TextSource: source}
	for _, m := range pageMarkRe.FindAllStringSubmatch(text, -1) { // номер последней страницы
		n := 0
		fmt.Sscan(m[1], &n)
		doc.Pages = max(doc.Pages, n)
	}
	if t := parseDate(date); t != nil {
		doc.AgeDays = int(analyticsNow().Sub(*t).Hours() / 24)
		doc.Stale = doc.AgeDays > getenvInt("ANALYTICS_STALE_DAYS", 180)
	}
	kb.Docs = append(kb.Docs, doc)

	// Текст режется по страницам (чтобы каждый фрагмент можно было процитировать со ссылкой «стр. N»),
	// внутри страницы — по абзацам, ~900 символов на фрагмент с перекрытием в один абзац.
	type pageText struct {
		n    int
		text string
	}
	var pages []pageText
	locs := pageMarkRe.FindAllStringSubmatchIndex(text, -1)
	if len(locs) == 0 {
		pages = []pageText{{0, text}}
	} else {
		for i, l := range locs {
			n := 0
			fmt.Sscan(text[l[2]:l[3]], &n)
			end := len(text)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			pages = append(pages, pageText{n, text[l[1]:end]})
		}
	}
	n := 0
	for _, pg := range pages {
		var paras []string
		for _, p := range paraSplitRe.Split(pg.text, -1) {
			p = strings.TrimSpace(spaceRe.ReplaceAllString(p, " "))
			// Заголовки, метаданные, служебные пометки («> ...») и дисклеймеры в фрагменты не попадают.
			if runeLen(p) < 30 || metaDateRe.MatchString(p) || metaFileRe.MatchString(p) || metaAuthorRe.MatchString(p) ||
				strings.HasPrefix(p, "#") || strings.HasPrefix(p, ">") || countAny(p, boilerplateTerms) >= 1 {
				continue
			}
			// Длинный абзац делится по предложениям.
			if runeLen(p) > 1100 {
				var cur strings.Builder
				for _, s := range splitSentences(p) {
					if cur.Len() > 0 && runeLen(cur.String())+runeLen(s) > 900 {
						paras = append(paras, cur.String())
						cur.Reset()
					}
					if cur.Len() > 0 {
						cur.WriteString(" ")
					}
					cur.WriteString(s)
				}
				if cur.Len() > 0 {
					paras = append(paras, cur.String())
				}
				continue
			}
			paras = append(paras, p)
		}
		var cur []string
		size := 0
		flush := func() {
			if len(cur) == 0 {
				return
			}
			n++
			body := strings.Join(cur, "\n")
			ch := Chunk{ID: fmt.Sprintf("%s.%d", docID, n), DocID: docID, DocTitle: title, Date: date, File: file, Page: pg.n,
				Text: body, Synthetic: synthetic, Stale: doc.Stale}
			ev := &Event{Title: title, Text: body}
			ClassifyEvent(ev, nil)
			ch.Factors, ch.Sectors = ev.FactorKeys(), ev.Sectors
			ch.Table = len(numRe.FindAllString(body, -1)) > runeLen(body)/25
			kb.Chunks = append(kb.Chunks, ch)
		}
		for _, p := range paras {
			if size+runeLen(p) > 900 && len(cur) > 0 {
				flush()
				last := cur[len(cur)-1]
				cur, size = []string{last}, runeLen(last)
				if size > 450 { // длинный абзац не дублируем целиком
					cur, size = nil, 0
				}
			}
			cur = append(cur, p)
			size += runeLen(p)
		}
		flush()
	}
}

// DocByID — документ по идентификатору «A3».
func (kb *KnowledgeBase) DocByID(id string) *AnalyticsDoc {
	for i := range kb.Docs {
		if kb.Docs[i].ID == id {
			return &kb.Docs[i]
		}
	}
	return nil
}

// Search — гибридный поиск: семантическая близость + совпадение факторов/отраслей.
func (kb *KnowledgeBase) Search(query string, factorKeys, sectorKeys []string, k int) ([]ChunkHit, error) {
	if len(kb.Chunks) == 0 {
		return nil, nil
	}
	texts := make([]string, len(kb.Chunks))
	for i, c := range kb.Chunks {
		texts[i] = c.DocTitle + ". " + c.Text
	}
	// TF-IDF строится на корпусе вместе с запросом; для BGE — векторы кэшируются.
	var docVecs, qVecs []Vector
	var err error
	if _, isTfidf := kb.vec.(TfidfBackend); isTfidf {
		docVecs, qVecs, err = kb.vec.Embed(texts, []string{query})
	} else {
		kb.mu.Lock()
		if kb.vecs == nil {
			kb.vecs, _, err = kb.vec.Embed(texts, nil)
		}
		docVecs = kb.vecs
		kb.mu.Unlock()
		if err == nil {
			_, qVecs, err = kb.vec.Embed(nil, []string{query})
		}
	}
	if err != nil {
		return nil, err
	}
	sims := cosineMatrix(docVecs, qVecs)
	hits := make([]ChunkHit, 0, len(kb.Chunks))
	for i, c := range kb.Chunks {
		score := sims[i][0]
		// Совпадение факторов: главный фактор события (первый) весит больше; таблицы, где
		// упомянуто всё сразу, не должны обгонять текст по сути вопроса.
		fb := 0.0
		for i, f := range factorKeys {
			if contains(c.Factors, f) {
				if i == 0 {
					fb += 0.15
				} else {
					fb += 0.04
				}
			}
		}
		score += math.Min(fb, 0.2)
		for _, s := range sectorKeys {
			if contains(c.Sectors, s) {
				score += 0.08
			}
		}
		// Актуальность: свежий материал банка важнее старого, устаревший (2021 г.) — ниже,
		// чтобы позиция банка опиралась на последний прогноз, а не на пересмотренный.
		if t := parseDate(c.Date); t != nil {
			score += 0.06 * freshness(t, analyticsNow(), 45)
		}
		if c.Stale {
			score *= 0.55
		}
		if c.Table {
			score *= 0.75
		}
		hits = append(hits, ChunkHit{Chunk: c, Score: round(score, 4)})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	// Не больше двух фрагментов из одного документа — разнообразие источников.
	var out []ChunkHit
	perDoc := map[string]int{}
	for _, h := range hits {
		if h.Score < 0.12 || perDoc[h.DocID] >= 2 {
			continue
		}
		perDoc[h.DocID]++
		out = append(out, h)
		if len(out) >= k {
			break
		}
	}
	return out, nil
}

// CoverageFactors — по каким факторам у банка есть аналитика (для ранжирования).
func (kb *KnowledgeBase) CoverageFactors() map[string]bool {
	out := map[string]bool{}
	for _, c := range kb.Chunks {
		for _, f := range c.Factors {
			out[f] = true
		}
	}
	return out
}

// enrichPDFWithVision — страницы PDF, где почти нет текста (графики, сканы), расшифровываются
// vision-моделью Qwen3-VL. Результат кэшируется на диске: каждую страницу оплачиваем один раз.
func enrichPDFWithVision(path string, pages []string) []string {
	maxPages := getenvInt("ANALYTICS_VISION_MAX_PAGES", 15)
	data, err := os.ReadFile(path)
	if err != nil {
		return pages
	}
	fileHash := shortHash(string(data))
	cacheDir := filepath.Join(cfg.StateDir, "vision_cache")
	_ = os.MkdirAll(cacheDir, 0o755)
	used := 0
	for i, pg := range pages {
		if runeLen(strings.TrimSpace(pg)) >= 300 || used >= maxPages {
			continue
		}
		cache := filepath.Join(cacheDir, fmt.Sprintf("%s_p%d.txt", fileHash, i+1))
		if b, err := os.ReadFile(cache); err == nil {
			pages[i] = pg + "\n" + string(b)
			continue
		}
		tmp, err := os.MkdirTemp("", "pdfpage")
		if err != nil {
			continue
		}
		prefix := filepath.Join(tmp, "p")
		n := fmt.Sprint(i + 1)
		if err := exec.Command("pdftoppm", "-f", n, "-l", n, "-r", "110", "-png", "-singlefile", path, prefix).Run(); err != nil {
			os.RemoveAll(tmp)
			continue
		}
		png, err := os.ReadFile(prefix + ".png")
		os.RemoveAll(tmp)
		if err != nil {
			continue
		}
		used++
		res, err := VisionTranscribe(png, "analytics-loader")
		if err != nil {
			logWarn("vision-расшифровка %s, стр. %d: %v", filepath.Base(path), i+1, err)
			continue
		}
		_ = os.WriteFile(cache, []byte(res.Text), 0o644)
		pages[i] = pg + "\n" + res.Text
		logInfo("%s, стр. %d расшифрована моделью %s", filepath.Base(path), i+1, res.Model)
	}
	return pages
}
