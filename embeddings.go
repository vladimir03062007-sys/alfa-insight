// Векторизация текстов (эмбеддинги):
//   - TF-IDF (по умолчанию) — работает без нейросети и без внешних сервисов;
//   - BGE-M3 — опционально, локальная embedding-модель через Ollama
//     (или любой OpenAI-совместимый сервер эмбеддингов). Перед использованием
//     уточните у организаторов, допустима ли embedding-модель вне списка LLM.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Vector — один эмбеддинг.
type Vector = []float64

// VectorizerBackend — общий интерфейс для BGE-M3 и TF-IDF.
// Embed возвращает векторы документов и векторы запросов в одном пространстве.
type VectorizerBackend interface {
	Name() string
	Embed(documents, queries []string) (docs, qs []Vector, err error)
}

// ---------- косинусное сходство ----------

func norm(v Vector) float64 {
	s := 0.0
	for _, x := range v {
		s += x * x
	}
	n := math.Sqrt(s)
	if n == 0 {
		return 1
	}
	return n
}

// cosineMatrix — матрица сходств: [i][j] = cos(a[i], b[j]).
func cosineMatrix(a, b []Vector) [][]float64 {
	out := make([][]float64, len(a))
	bn := make([]float64, len(b))
	for j := range b {
		bn[j] = norm(b[j])
	}
	for i := range a {
		out[i] = make([]float64, len(b))
		an := norm(a[i])
		for j := range b {
			dot := 0.0
			n := len(a[i])
			if len(b[j]) < n {
				n = len(b[j])
			}
			for k := 0; k < n; k++ {
				dot += a[i][k] * b[j][k]
			}
			out[i][j] = dot / (an * bn[j])
		}
	}
	return out
}

// ---------- TF-IDF (повторяет поведение sklearn TfidfVectorizer) ----------

type TfidfBackend struct{}

func (TfidfBackend) Name() string { return "tfidf" }

// tokenize — аналог регулярки sklearn (?u)\b\w\w+\b после lowercase:
// берём непрерывные последовательности букв/цифр/_ длиной от 2 символов.
func tokenize(text string) []string {
	var tokens []string
	var cur []rune
	flush := func() {
		if len(cur) >= 2 {
			// Основа слова: «ставки», «ставку», «ставка» → «ставк» (см. morph.go).
			tokens = append(tokens, stemRu(string(cur)))
		}
		cur = cur[:0]
	}
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || unicode.Is(unicode.Mn, r) {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	return tokens
}

// ngrams — униграммы и биграммы (ngram_range=(1, 2)).
func ngrams(tokens []string) []string {
	out := append([]string{}, tokens...)
	for i := 0; i+1 < len(tokens); i++ {
		out = append(out, tokens[i]+" "+tokens[i+1])
	}
	return out
}

func (TfidfBackend) Embed(documents, queries []string) ([]Vector, []Vector, error) {
	corpus := append(append([]string{}, documents...), queries...)
	if len(corpus) == 0 {
		return []Vector{}, []Vector{}, nil
	}
	const maxFeatures = 12000

	// 1. Подсчёт терминов в каждом документе.
	counts := make([]map[string]int, len(corpus))
	total := map[string]int{} // суммарная частота термина (для max_features)
	df := map[string]int{}    // в скольких документах встречается термин
	for i, doc := range corpus {
		c := map[string]int{}
		for _, t := range ngrams(tokenize(doc)) {
			c[t]++
		}
		counts[i] = c
		for t, n := range c {
			total[t] += n
			df[t]++
		}
	}

	// 2. Словарь: если терминов больше лимита — оставляем самые частые.
	vocab := make([]string, 0, len(total))
	for t := range total {
		vocab = append(vocab, t)
	}
	sort.Strings(vocab)
	if len(vocab) > maxFeatures {
		sort.SliceStable(vocab, func(i, j int) bool { return total[vocab[i]] > total[vocab[j]] })
		vocab = vocab[:maxFeatures]
		sort.Strings(vocab)
	}
	if len(vocab) == 0 {
		vocab = []string{""} // пустой словарь — нулевые векторы размерности 1
	}
	index := make(map[string]int, len(vocab))
	for i, t := range vocab {
		index[t] = i
	}

	// 3. idf со сглаживанием: ln((1+n)/(1+df)) + 1.
	n := float64(len(corpus))
	idf := make([]float64, len(vocab))
	for i, t := range vocab {
		idf[i] = math.Log((1+n)/(1+float64(df[t]))) + 1
	}

	// 4. Векторы: сублинейный tf (1 + ln tf) * idf, затем L2-нормализация.
	matrix := make([]Vector, len(corpus))
	for i, c := range counts {
		v := make(Vector, len(vocab))
		for t, cnt := range c {
			if j, ok := index[t]; ok {
				v[j] = (1 + math.Log(float64(cnt))) * idf[j]
			}
		}
		nv := norm(v)
		for j := range v {
			v[j] /= nv
		}
		matrix[i] = v
	}
	return matrix[:len(documents)], matrix[len(documents):], nil
}

// ---------- BGE-M3 ----------

// BGEM3Backend — локальная мультиязычная модель BAAI/bge-m3.
// Модель крутится в Ollama (`ollama pull bge-m3`), программа обращается к ней по HTTP.
// API-ключ не нужен, токены не списываются.
type BGEM3Backend struct {
	ModelName string
	Endpoint  string
	API       string // ollama | openai
	BatchSize int
	MaxChars  int
	client    *http.Client
}

func NewBGEM3Backend() *BGEM3Backend {
	return &BGEM3Backend{
		ModelName: cfg.BGEModel,
		Endpoint:  cfg.BGEEndpoint,
		API:       cfg.BGEAPI,
		BatchSize: cfg.BGEBatchSize,
		MaxChars:  cfg.BGEMaxChars,
		client:    &http.Client{Timeout: 10 * time.Minute},
	}
}

func (b *BGEM3Backend) Name() string { return "bge-m3" }

// prepare обрезает тексты, чтобы расход памяти был предсказуемым.
func (b *BGEM3Backend) prepare(texts []string) []string {
	out := make([]string, len(texts))
	for i, t := range texts {
		out[i] = truncRunes(t, b.MaxChars)
		if strings.TrimSpace(out[i]) == "" {
			out[i] = " " // некоторые серверы не принимают пустую строку
		}
	}
	return out
}

func (b *BGEM3Backend) post(url string, payload any, out any) error {
	body, _ := json.Marshal(payload)
	resp, err := b.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("BGE-M3 недоступна по адресу %s: %w\n"+
			"Запустите Ollama и скачайте модель: ollama pull %s "+
			"(или используйте --embedding tfidf)", b.Endpoint, err, b.ModelName)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("сервер эмбеддингов вернул %d: %s", resp.StatusCode, truncRunes(string(data), 300))
	}
	return json.Unmarshal(data, out)
}

// encodeBatch отправляет одну пачку текстов на сервер эмбеддингов.
func (b *BGEM3Backend) encodeBatch(texts []string) ([]Vector, error) {
	if b.API == "openai" {
		// OpenAI-совместимый API (TEI, llama.cpp server, LM Studio, vLLM...).
		var r struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		if err := b.post(b.Endpoint+"/embeddings", map[string]any{"model": b.ModelName, "input": texts}, &r); err != nil {
			return nil, err
		}
		sort.Slice(r.Data, func(i, j int) bool { return r.Data[i].Index < r.Data[j].Index })
		out := make([]Vector, len(r.Data))
		for i, d := range r.Data {
			out[i] = d.Embedding
		}
		return out, nil
	}
	// Ollama: POST /api/embed {"model": "...", "input": [...]}
	var r struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := b.post(b.Endpoint+"/api/embed", map[string]any{"model": b.ModelName, "input": texts}, &r); err != nil {
		return nil, err
	}
	return r.Embeddings, nil
}

func (b *BGEM3Backend) encode(texts []string) ([]Vector, error) {
	if len(texts) == 0 {
		return []Vector{}, nil
	}
	prepared := b.prepare(texts)
	var out []Vector
	for start := 0; start < len(prepared); start += b.BatchSize {
		end := start + b.BatchSize
		if end > len(prepared) {
			end = len(prepared)
		}
		vecs, err := b.encodeBatch(prepared[start:end])
		if err != nil {
			return nil, err
		}
		if len(vecs) != end-start {
			return nil, fmt.Errorf("сервер эмбеддингов вернул %d векторов вместо %d", len(vecs), end-start)
		}
		out = append(out, vecs...)
	}
	// Нормализуем векторы (normalize_embeddings=True).
	for _, v := range out {
		n := norm(v)
		for j := range v {
			v[j] /= n
		}
	}
	return out, nil
}

func (b *BGEM3Backend) Embed(documents, queries []string) ([]Vector, []Vector, error) {
	logInfo("Эмбеддинги %s: %d документов, %d запросов", b.ModelName, len(documents), len(queries))
	docs, err := b.encode(documents)
	if err != nil {
		return nil, nil, err
	}
	qs, err := b.encode(queries)
	if err != nil {
		return nil, nil, err
	}
	return docs, qs, nil
}

// GetBackend выбирает backend по имени: auto/bge/bge-m3/bgem3 или tfidf.
func GetBackend(name string) (VectorizerBackend, error) {
	name = strings.ToLower(name)
	if name == "" || name == "auto" {
		name = "tfidf"
	}
	switch name {
	case "bge", "bge-m3", "bgem3":
		return NewBGEM3Backend(), nil
	case "tfidf":
		return TfidfBackend{}, nil
	}
	return nil, fmt.Errorf("неизвестный embedding backend: %s", name)
}
