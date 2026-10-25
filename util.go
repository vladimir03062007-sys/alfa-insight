// Общие помощники: логирование, JSON, строки, даты, числа.
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ---------- логирование ----------

var verbose bool

func logInfo(format string, args ...any) {
	if verbose {
		fmt.Fprintf(os.Stderr, "INFO: "+format+"\n", args...)
	}
}

func logDebug(format string, args ...any) {
	if os.Getenv("APP_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "DEBUG: "+format+"\n", args...)
	}
}

func logWarn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "WARNING: "+format+"\n", args...)
}

// ---------- JSON ----------

// marshalPretty — JSON с отступом и без экранирования кириллицы и <>&.
func marshalPretty(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := marshalPretty(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// ---------- строки ----------

func runeLen(s string) int { return len([]rune(s)) }

// truncRunes — обрезка строки по символам (а не байтам).
func truncRunes(s string, n int) string {
	r := []rune(s)
	if n < 0 {
		n = 0
	}
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// shorten — обрезка по границе слова с многоточием.
func shorten(s string, n int) string {
	s = strings.TrimSpace(s)
	if runeLen(s) <= n {
		return s
	}
	cut := truncRunes(s, n-1)
	if i := strings.LastIndex(cut, " "); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,.;:—-") + "…"
}

func contains(list []string, x string) bool {
	for _, v := range list {
		if v == x {
			return true
		}
	}
	return false
}

// hasStem — термин встречается в тексте с начала слова («кредит» не найдётся в «аккредитованных»).
func hasStem(low, term string) bool {
	if term == "" {
		return false
	}
	for from := 0; ; {
		i := strings.Index(low[from:], term)
		if i < 0 {
			return false
		}
		pos := from + i
		if pos == 0 {
			return true
		}
		r, _ := utf8.DecodeLastRuneInString(low[:pos])
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return true
		}
		from = pos + len(term)
	}
}

// containsAny — встречается ли в тексте хотя бы один термин. Многословные термины
// сравниваются с учётом окончаний (см. morph.go): «о курсе рубля» находится по «курс рубл».
func containsAny(text string, terms []string) bool {
	return countAnyN(text, terms, 1) > 0
}

// countAny — сколько разных терминов из списка встречается в тексте.
func countAny(text string, terms []string) int {
	return countAnyN(text, terms, len(terms))
}

func countAnyN(text string, terms []string, stopAt int) int {
	low := strings.ReplaceAll(strings.ToLower(text), "ё", "е")
	var toks []string
	n := 0
	for _, t := range terms {
		if toks == nil && (strings.ContainsAny(t, " -/") || strings.HasSuffix(t, " ")) {
			toks = wordTokens(low)
		}
		if termIn(low, toks, strings.ReplaceAll(t, "ё", "е")) {
			n++
			if n >= stopAt {
				return n
			}
		}
	}
	return n
}

func uniqueStrings(xs []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, x := range xs {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func shortHash(parts ...string) string {
	sum := sha1.Sum([]byte(strings.Join(parts, "|")))
	return strings.ToUpper(hex.EncodeToString(sum[:]))[:10]
}

var sentenceSplitRe = regexp.MustCompile(`([.!?…])\s+`)

// splitSentences — грубое деление текста на предложения.
func splitSentences(text string) []string {
	text = strings.TrimSpace(spaceRe.ReplaceAllString(text, " "))
	if text == "" {
		return nil
	}
	// Точка после сокращения («2026 г. ожидаем», «42 млрд руб. по номиналу», «дек./дек. — 5,6%»)
	// не конец предложения: новое предложение начинается с заглавной буквы, цифры или кавычки.
	var b strings.Builder
	last := 0
	for _, m := range sentenceSplitRe.FindAllStringIndex(text, -1) {
		next, _ := utf8.DecodeRuneInString(text[m[1]:])
		b.WriteString(text[last:m[1]])
		if unicode.IsUpper(next) || unicode.IsDigit(next) || next == '«' || next == '"' {
			b.WriteString("\x00")
		}
		last = m[1]
	}
	b.WriteString(text[last:])
	marked := b.String()
	var out []string
	for _, s := range strings.Split(marked, "\x00") {
		if s = strings.TrimSpace(s); runeLen(s) > 2 {
			out = append(out, s)
		}
	}
	return out
}

func capitalize(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// ---------- числа ----------

func clamp(x, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, x)) }

func round(x float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(x*p) / p
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// parseNumber понимает «1 234,5», «1234.5», «15e9», пустые значения -> 0, false.
func parseNumber(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, " ", "")
	if s == "" || strings.EqualFold(s, "nan") || strings.EqualFold(s, "null") {
		return 0, false
	}
	if strings.Count(s, ",") == 1 && !strings.Contains(s, ".") {
		s = strings.Replace(s, ",", ".", 1)
	} else {
		s = strings.ReplaceAll(s, ",", "")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// formatRub — «23,4 млрд ₽».
func formatRub(v float64) string {
	abs := math.Abs(v)
	switch {
	case abs >= 1e12:
		return strings.Replace(fmt.Sprintf("%.1f трлн ₽", v/1e12), ".", ",", 1)
	case abs >= 1e9:
		return strings.Replace(fmt.Sprintf("%.1f млрд ₽", v/1e9), ".", ",", 1)
	case abs >= 1e6:
		return strings.Replace(fmt.Sprintf("%.1f млн ₽", v/1e6), ".", ",", 1)
	case abs >= 1e3:
		return strings.Replace(fmt.Sprintf("%.1f тыс. ₽", v/1e3), ".", ",", 1)
	}
	return fmt.Sprintf("%.0f ₽", v)
}

// ---------- даты ----------

func dtToStr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

var dateLayouts = []string{
	time.RFC3339Nano, time.RFC3339,
	"2006-01-02T15:04:05-0700", "2006-01-02T15:04:05", "2006-01-02T15:04",
	"2006-01-02 15:04:05-07:00", "2006-01-02 15:04:05 -0700", "2006-01-02 15:04:05",
	"2006-01-02 15:04", "2006-01-02",
	time.RFC1123Z, time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST",
	"Mon, 02 Jan 2006 15:04 -0700", "Mon, 2 Jan 2006 15:04:05",
	"2 Jan 2006 15:04:05 -0700", "02 Jan 2006 15:04:05 MST",
	time.RFC850, time.ANSIC, time.UnixDate,
	"January 2, 2006", "Jan 2, 2006", "02.01.2006 15:04", "02.01.2006",
}

// parseDate пробует все известные форматы. Если пояс не указан — UTC.
func parseDate(value string) *time.Time {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil
	}
	if strings.HasSuffix(v, "Z") && !strings.Contains(v, " ") {
		v = strings.TrimSuffix(v, "Z") + "+00:00"
	}
	for _, layout := range dateLayouts {
		if t, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
			return &t
		}
	}
	return nil
}

var ruMonths = []string{"января", "февраля", "марта", "апреля", "мая", "июня", "июля",
	"августа", "сентября", "октября", "ноября", "декабря"}

func formatDateRu(t *time.Time) string {
	if t == nil {
		return "дата не указана"
	}
	return fmt.Sprintf("%d %s %d", t.Day(), ruMonths[t.Month()-1], t.Year())
}

// jsonUnmarshalFlexible — массив или объект с ключом items/publications.
func jsonUnmarshalFlexible(data []byte, out any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return err
		}
		for _, k := range []string{"items", "publications", "news"} {
			if v, ok := obj[k]; ok {
				return json.Unmarshal(v, out)
			}
		}
		return fmt.Errorf("ожидался массив или объект с ключом items")
	}
	return json.Unmarshal(trimmed, out)
}

func osReadDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}
