// Прогнозы банка, извлечённые из аналитики: ключевая ставка, инфляция, курс, ВВП на конец года.
//
// Зачем: материалы банка выходят в разные даты, и позиция меняется. В июле банк ждал ставку
// 13,5% на конец 2026 г., а после заседания 11 сентября — 14,0%. Если RAG случайно процитирует
// старый материал, руководитель получит устаревшую позицию. Поэтому сервис:
//   - извлекает числовые прогнозы из каждого фрагмента (с датой документа и ссылкой на фрагмент);
//   - показывает актуальный прогноз (из самого свежего материала) и историю пересмотров;
//   - помечает в инсайте, если процитированный фрагмент содержит уже пересмотренный прогноз.
//
// Извлечение — правила (регулярные выражения), не LLM: результат воспроизводим и проверяем,
// каждое число имеет ссылку на фрагмент-источник (A<doc>.<chunk>).
package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type Forecast struct {
	Indicator string  `json:"indicator"`      // key_rate | inflation | usd_rub | cny_rub | gdp
	Name      string  `json:"name"`           // «Ключевая ставка на конец года»
	Period    string  `json:"period"`         // «2026»
	Value     float64 `json:"value"`          // 14.0
	Unit      string  `json:"unit"`           // «%», «руб./$»
	DocID     string  `json:"doc_id"`         // A2
	ChunkID   string  `json:"chunk_id"`       // A2.5
	DocTitle  string  `json:"doc_title"`      //
	DocDate   string  `json:"doc_date"`       // 2026-09-11
	Page      int     `json:"page,omitempty"` //
	Quote     string  `json:"quote"`          // фрагмент текста, откуда взято число
}

// Связь индикатора с фактором таксономии (для подбора прогноза к событию).
var indicatorFactor = map[string]string{"key_rate": "key_rate", "inflation": "inflation", "usd_rub": "fx_rub", "cny_rub": "fx_rub", "gdp": "gdp_growth"}

var indicatorNames = map[string]string{
	"key_rate": "Ключевая ставка на конец года", "inflation": "Инфляция на конец года (дек./дек.)",
	"usd_rub": "Курс рубля к доллару на конец года", "cny_rub": "Курс рубля к юаню на конец года", "gdp": "Рост ВВП за год",
}

// Ключевые слова показателей. Показатель для числа определяется по ближайшему слову слева,
// а не по всему предложению: в «прогноз по инфляции … 7,5%, что сдерживает снижение ключевой ставки»
// число 7,5% относится к инфляции.
var indicatorCues = []struct{ ind, cue string }{
	{"key_rate", "ставк"}, {"key_rate", "кс "},
	{"inflation", "инфляц"},
	{"usd_rub", "руб./$"}, {"usd_rub", "доллар"}, {"usd_rub", "курс"},
	{"cny_rub", "руб./юань"}, {"cny_rub", "юан"},
	{"gdp", "ввп"},
}

// Слова, при которых число — не прогноз показателя на конец года.
var indicatorStop = map[string][]string{
	"key_rate":  {"средн", "ипотек", "депозит", "доходност", "купон", "фрахт", "ставки по кредит", "ставок по кредит", "денежного рынка", "фрс", "ruonia", "сша"},
	"inflation": {"ожидани", "сша", "cpi", "pce", "базов", "недельн", "н/н", "среднегод", "м/м"},
	"usd_rub":   {},
	"cny_rub":   {},
	"gdp":       {"номинальн", "сша", "трлн", "квартал", "2к", "1к"},
}

// Прошлый взгляд банка в тексте нового материала («Если раньше мы ориентировались на 13,5%…»).
var pastViewCues = []string{"раньше", "ранее", "прежн", "предыдущ", "до этого", "первоначальн"}

// indicatorNear — показатель, к которому относится число в позиции pos строки s.
func indicatorNear(s string, pos int) string {
	from := max(0, pos-110)
	for from > 0 && !utf8RuneStart(s[from]) {
		from--
	}
	win := strings.ToLower(s[from:pos])
	best, bestPos := "", -1
	for _, c := range indicatorCues {
		if i := strings.LastIndex(win, c.cue); i > bestPos {
			best, bestPos = c.ind, i
		}
	}
	if best == "" {
		return ""
	}
	// Стоп-слова проверяем между ключевым словом и числом.
	tail := win[bestPos:]
	if containsAny(tail, indicatorStop[best]) || containsAny(tail, pastViewCues) {
		return ""
	}
	return best
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

func unitFor(ind, raw string) string {
	if raw != "" {
		return raw
	}
	switch ind {
	case "usd_rub":
		return "руб./$"
	case "cny_rub":
		return "руб./юань"
	}
	return "%"
}

func plausible(ind string, v float64) bool {
	switch ind {
	case "key_rate":
		return v >= 3 && v <= 30
	case "inflation":
		return v >= 0 && v <= 30
	case "usd_rub":
		return v >= 40 && v <= 200
	case "cny_rub":
		return v >= 5 && v <= 30
	case "gdp":
		return v >= -15 && v <= 15
	}
	return false
}

var (
	fcNum  = `(\d{1,3}(?:,\d{1,2})?)`
	fcUnit = `\s*(%|руб\./\$|руб\./юань)`
	// «на конец 2026 г. ожидаем ставку на уровне 13,5%», «на конец 2026П — 14,0%»
	fcEndYearRe = regexp.MustCompile(`(?:на конец|по итогам)\s+(20\d\d)\s*(?:г\.?|года|П)?[^%;]{0,60}?` + fcNum + fcUnit)
	// «13,5% на конец 2026 г.», «7,5% г/г по итогам текущего года»
	fcValFirstRe = regexp.MustCompile(fcNum + fcUnit + `\s*(?:г/г\s*)?(?:по итогам|на конец)\s+(20\d\d|текущего года|этого года|года)`)
	// «14,0% до конца года»
	fcTillEndRe = regexp.MustCompile(fcNum + fcUnit + `\s*до конца\s+(года|20\d\d)`)
	// Таблицы: «… на конец года: 2025 — 16,0%; 2026П — 14,0%; 2027П — 12,0%»
	fcTableRe = regexp.MustCompile(`(20\d\d)(П?)\s*—\s*` + fcNum + `\s*(%|руб\./\$|руб\./юань)?`)
	// Таблицы со слешами: заголовок «2025 / 2026П / 2027П», строки «… — 16,0% / 13,5% / 12,0%».
	fcSlashHeadRe = regexp.MustCompile(`(20\d\d)\s*/\s*(20\d\d)П?\s*/\s*(20\d\d)П?`)
	fcSlashRowRe  = regexp.MustCompile(`([^;:]+?)\s—\s*(-?\d{1,3}(?:,\d{1,2})?)\s*(%|руб\./\$|руб\./юань)?\s*/\s*(-?\d{1,3}(?:,\d{1,2})?)\s*(?:%|руб\./\$|руб\./юань)?\s*/\s*(-?\d{1,3}(?:,\d{1,2})?)`)
)

// ExtractForecasts — прогнозы банка из всех фрагментов. Берутся только будущие периоды
// (год прогноза ≥ года документа) и только взгляд, актуальный на дату документа.
// Если в документе показатель встречается несколько раз, таблица прогнозов важнее текста.
func ExtractForecasts(kb *KnowledgeBase) []Forecast {
	type cand struct {
		f    Forecast
		prio int
	}
	best := map[string]cand{}
	var order []string
	add := func(c Chunk, ind, year, val, unit, quote string, prio int) {
		v, ok := parseNumber(val)
		if !ok || ind == "" || !plausible(ind, v) {
			return
		}
		if ind == "gdp" && strings.Contains(val, ",") == false && v > 5 { // «на 2026 год» не путать с процентами
			return
		}
		docYear := ""
		if len(c.Date) >= 4 {
			docYear = c.Date[:4]
		}
		switch year {
		case "текущего года", "этого года", "года":
			year = docYear
		}
		if docYear != "" && year < docYear { // факт прошлого года, а не прогноз
			return
		}
		if (ind == "usd_rub" || ind == "cny_rub") != (unit != "%") {
			return
		}
		key := c.DocID + "|" + ind + "|" + year
		f := Forecast{Indicator: ind, Name: indicatorNames[ind], Period: year, Value: v, Unit: unit,
			DocID: c.DocID, ChunkID: c.ID, DocTitle: c.DocTitle, DocDate: c.Date, Page: c.Page, Quote: shorten(strings.TrimSpace(quote), 240)}
		old, exists := best[key]
		if !exists {
			order = append(order, key)
		}
		if !exists || prio > old.prio {
			best[key] = cand{f, prio}
		}
	}
	// Цитата — окрестность числа (до 140 байт слева), а не всё длинное предложение.
	around := func(clause string, from, to int) string {
		start := max(0, from-140)
		for start > 0 && !utf8RuneStart(clause[start]) {
			start++
		}
		if i := strings.LastIndexAny(clause[:start+1], ",.;:"); start > 0 && i >= 0 && from-i < 200 {
			start = i + 1
		}
		end := min(len(clause), to+40)
		for end < len(clause) && !utf8RuneStart(clause[end]) {
			end++
		}
		q := strings.TrimSpace(clause[start:end])
		if start > 0 {
			q = "…" + q
		}
		return q
	}
	for _, c := range kb.Chunks {
		if c.Synthetic || c.Stale {
			continue
		}
		for _, sent := range splitSentences(c.Text) {
			low := strings.ToLower(sent)
			// 1) Таблица «2025 / 2026П / 2027П».
			if h := fcSlashHeadRe.FindStringSubmatch(sent); h != nil {
				for _, idx := range fcSlashRowRe.FindAllStringSubmatchIndex(sent, -1) {
					row := make([]string, len(idx)/2)
					for i := range row {
						if idx[2*i] >= 0 {
							row[i] = sent[idx[2*i]:idx[2*i+1]]
						}
					}
					// Единица измерения стоит после последнего числа строки: «78,2 / 87,2 / 95,0 руб./$».
					after := sent[idx[1]:min(len(sent), idx[1]+16)]
					if row[3] == "" {
						for _, u := range []string{"руб./$", "руб./юань", "%"} {
							if strings.HasPrefix(strings.TrimSpace(after), u) {
								row[3] = u
								break
							}
						}
					}
					label := strings.ToLower(row[1])
					ind := ""
					switch {
					case strings.Contains(label, "ключевая ставка на конец"):
						ind = "key_rate"
					case strings.Contains(label, "инфляция дек"):
						ind = "inflation"
					case strings.Contains(label, "курс на конец года") && row[3] == "руб./$":
						ind = "usd_rub"
					case strings.Contains(label, "курс на конец года") && row[3] == "руб./юань":
						ind = "cny_rub"
					case strings.Contains(label, "темпы роста ввп"):
						ind = "gdp"
					}
					if ind == "" {
						continue
					}
					for i, y := range []string{h[1], h[2], h[3]} {
						add(c, ind, y, row[[]int{2, 4, 5}[i]], unitFor(ind, row[3]),
							strings.TrimSpace(row[0])+" "+unitFor(ind, row[3])+" ("+h[0]+")", 3)
					}
				}
				continue
			}
			// 2) Табличная строка «Ключевая ставка на конец года: 2023 — 16,0%; … 2026П — 14,0%».
			if strings.Contains(low, "на конец года") || strings.Contains(low, "дек./дек") {
				for _, part := range strings.Split(sent, ". ") {
					pl := strings.ToLower(part)
					ind := ""
					switch {
					case strings.Contains(pl, "ставка на конец года") && !strings.Contains(pl, "средн"):
						ind = "key_rate"
					case strings.Contains(pl, "инфляция") && strings.Contains(pl, "дек"):
						ind = "inflation"
					case strings.Contains(pl, "курс на конец года") && strings.Contains(pl, "руб./$"):
						ind = "usd_rub"
					case strings.Contains(pl, "курс на конец года") && strings.Contains(pl, "руб./юань"):
						ind = "cny_rub"
					}
					if ind == "" {
						continue
					}
					for _, m := range fcTableRe.FindAllStringSubmatch(part, -1) {
						if m[2] == "П" || len(c.Date) >= 4 && m[1] > c.Date[:4] {
							add(c, ind, m[1], m[3], unitFor(ind, m[4]), part, 3)
						}
					}
				}
			}
			// 3) Текст: число рядом со словами «на конец 2026», «по итогам года», «до конца года».
			for _, clause := range strings.Split(sent, ";") {
				for _, m := range fcEndYearRe.FindAllStringSubmatchIndex(clause, -1) {
					numPos := m[4]
					add(c, indicatorNear(clause, numPos), clause[m[2]:m[3]], clause[m[4]:m[5]], clause[m[6]:m[7]], around(clause, m[0], m[1]), 1)
				}
				for _, m := range fcValFirstRe.FindAllStringSubmatchIndex(clause, -1) {
					add(c, indicatorNear(clause, m[2]), clause[m[6]:m[7]], clause[m[2]:m[3]], clause[m[4]:m[5]], around(clause, m[0], m[1]), 1)
				}
				for _, m := range fcTillEndRe.FindAllStringSubmatchIndex(clause, -1) {
					y := clause[m[6]:m[7]]
					if y == "года" {
						y = "текущего года"
					}
					add(c, indicatorNear(clause, m[2]), y, clause[m[2]:m[3]], clause[m[4]:m[5]], around(clause, m[0], m[1]), 2)
				}
			}
		}
	}
	var out []Forecast
	for _, k := range order {
		out = append(out, best[k].f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Indicator != out[j].Indicator {
			return out[i].Indicator < out[j].Indicator
		}
		if out[i].Period != out[j].Period {
			return out[i].Period < out[j].Period
		}
		return out[i].DocDate < out[j].DocDate
	})
	return out
}

// significant — существенное ли различие двух прогнозов (87 и 87,2 руб./$ — одно и то же).
func significant(unit string, a, b float64) bool {
	if unit == "%" {
		return a-b >= 0.1 || b-a >= 0.1
	}
	return a-b >= 1 || b-a >= 1
}

// ForecastTrack — история прогноза одного показателя на один период.
type ForecastTrack struct {
	Indicator string     `json:"indicator"`
	Name      string     `json:"name"`
	Period    string     `json:"period"`
	Unit      string     `json:"unit"`
	Current   Forecast   `json:"current"`   // из самого свежего материала
	History   []Forecast `json:"history"`   // по датам материалов
	Revised   bool       `json:"revised"`   // прогноз пересматривался
	Label     string     `json:"label"`     // «ключевая ставка на конец 2026 г.»
	Summary   string     `json:"summary"`   // «13,5% (31 июля) → 14,0% (11 сентября)»
	Direction string     `json:"direction"` // up | down | flat — куда пересмотрен
}

// Tracks — история прогнозов по показателям (опционально только для фактора).
func (kb *KnowledgeBase) Tracks(factor string) []ForecastTrack {
	groups := map[string][]Forecast{}
	var keys []string
	for _, f := range kb.Forecasts {
		if factor != "" && indicatorFactor[f.Indicator] != factor {
			continue
		}
		k := f.Indicator + "|" + f.Period
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], f)
	}
	indOrder := map[string]int{"key_rate": 0, "inflation": 1, "usd_rub": 2, "cny_rub": 3, "gdp": 4}
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := strings.SplitN(keys[i], "|", 2), strings.SplitN(keys[j], "|", 2)
		if a[0] != b[0] {
			return indOrder[a[0]] < indOrder[b[0]]
		}
		return a[1] < b[1]
	})
	var out []ForecastTrack
	for _, k := range keys {
		h := groups[k]
		sort.SliceStable(h, func(i, j int) bool { return h[i].DocDate < h[j].DocDate })
		cur := h[len(h)-1]
		t := ForecastTrack{Indicator: cur.Indicator, Name: cur.Name, Period: cur.Period, Unit: cur.Unit, Current: cur, History: h,
			Direction: "flat", Label: ForecastLabel(cur.Indicator, cur.Period)}
		var parts []string
		prev := -1e9
		for _, f := range h {
			if prev == -1e9 || significant(f.Unit, f.Value, prev) {
				parts = append(parts, fmt.Sprintf("%s%s (%s)", fmtNum(f.Value), unitSuffix(f.Unit), dateShort(f.DocDate)))
				prev = f.Value
			}
		}
		first := h[0].Value
		if significant(cur.Unit, cur.Value, first) {
			t.Revised = true
			t.Direction = map[bool]string{true: "up", false: "down"}[cur.Value > first]
		}
		t.Summary = strings.Join(parts, " → ")
		out = append(out, t)
	}
	return out
}

// StaleClaims — какие прогнозы в процитированных фрагментах уже пересмотрены более свежими материалами.
// factors — ограничить факторами события (пусто — все).
func (kb *KnowledgeBase) StaleClaims(chunkIDs []string, factors ...string) []string {
	var out []string
	for _, t := range kb.Tracks("") {
		if len(factors) > 0 && !contains(factors, indicatorFactor[t.Indicator]) {
			continue
		}
		for _, f := range t.History {
			if contains(chunkIDs, f.ChunkID) && f.DocDate < t.Current.DocDate && significant(f.Unit, f.Value, t.Current.Value) {
				out = append(out, fmt.Sprintf("Прогноз «%s» в материале от %s (%s%s) уже пересмотрен: в материале от %s — %s%s [%s].",
					ForecastLabel(t.Indicator, t.Period), dateShort(f.DocDate), fmtNum(f.Value), unitSuffix(f.Unit),
					dateShort(t.Current.DocDate), fmtNum(t.Current.Value), unitSuffix(t.Current.Unit), t.Current.ChunkID))
			}
		}
	}
	return uniqueStrings(out)
}

// ForecastLabel — «ключевая ставка на конец 2026 г.».
func ForecastLabel(ind, period string) string {
	switch ind {
	case "key_rate":
		return "ключевая ставка на конец " + period + " г."
	case "inflation":
		return "инфляция на конец " + period + " г."
	case "usd_rub":
		return "курс рубля к доллару на конец " + period + " г."
	case "cny_rub":
		return "курс рубля к юаню на конец " + period + " г."
	case "gdp":
		return "рост ВВП в " + period + " г."
	}
	return ind + " " + period
}

func fmtNum(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return strings.ReplaceAll(s, ".", ",")
}

func unitSuffix(u string) string {
	if u == "%" {
		return "%"
	}
	return " " + u
}

func dateShort(d string) string {
	if t := parseDate(d); t != nil {
		return formatDateRu(t)
	}
	return d
}

// ---------- календарь: ближайшие заседания по ставке из материалов банка ----------

type CalendarItem struct {
	Date    string `json:"date"`   // 2026-10-23
	Org     string `json:"org"`    // «Банк России» | «ФРС»
	Factor  string `json:"factor"` // key_rate
	Text    string `json:"text"`   // «заседание Банка России по ключевой ставке»
	ChunkID string `json:"chunk_id"`
}

var calDateRe = regexp.MustCompile(`(\d{1,2})(?:\s*[–-]\s*(\d{1,2}))?\s+(января|февраля|марта|апреля|мая|июня|июля|августа|сентября|октября|ноября|декабря)`)

// Calendar — даты будущих заседаний ЦБ/ФРС, упомянутые в материалах (позже даты самого материала).
func (kb *KnowledgeBase) Calendar() []CalendarItem {
	seen := map[string]bool{}
	var out []CalendarItem
	for _, c := range kb.Chunks {
		docT := parseDate(c.Date)
		if docT == nil || c.Stale {
			continue
		}
		for _, s := range splitSentences(c.Text) {
			low := strings.ToLower(s)
			if !strings.Contains(low, "заседани") && !strings.Contains(low, "fomc") {
				continue
			}
			org, factor := "", ""
			switch {
			case strings.Contains(s, "ФРС") || strings.Contains(s, "FOMC"):
				org, factor = "ФРС", "gold"
			case strings.Contains(s, "ЦБ") || strings.Contains(s, "Банк") || strings.Contains(low, "ключев"):
				org, factor = "Банк России", "key_rate"
			default:
				continue
			}
			for _, m := range calDateRe.FindAllStringSubmatch(s, -1) {
				day := m[1]
				if m[2] != "" {
					day = m[2] // «15–16 сентября» — решение в последний день
				}
				month := 0
				for i, name := range ruMonths {
					if name == m[3] {
						month = i + 1
					}
				}
				dayN := 0
				fmt.Sscan(day, &dayN)
				d := fmt.Sprintf("%04d-%02d-%02d", docT.Year(), month, dayN)
				t := parseDate(d)
				if t == nil || !t.After(*docT) || seen[org+d] {
					continue
				}
				seen[org+d] = true
				out = append(out, CalendarItem{Date: d, Org: org, Factor: factor, ChunkID: c.ID,
					Text: "заседание " + map[string]string{"ФРС": "ФРС (FOMC)", "Банк России": "Банка России по ключевой ставке"}[org]})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

// ChunkByID — фрагмент аналитики по идентификатору «A2.7».
func (kb *KnowledgeBase) ChunkByID(id string) (Chunk, bool) {
	for _, c := range kb.Chunks {
		if c.ID == id {
			return c, true
		}
	}
	return Chunk{}, false
}
