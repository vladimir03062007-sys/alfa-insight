// Классификация новостей и выделение ключевой информации (без нейросети):
// факторы и направление их изменения, отрасли, регионы, участники, ключевые факты.
// В LLM-режиме агент может уточнить эту разметку, но базовая разметка
// всегда есть — сервис работает и без LLM, и при её недоступности.
package main

import (
	"regexp"
	"sort"
	"strings"
)

var (
	numberFactRe = regexp.MustCompile(`\d+([.,]\d+)?\s?(%|п\.п\.|б\.п\.|млрд|млн|трлн|тыс|руб|₽|\$|долл|юан|евро|т\b|тонн|баррел)`)
	dateFactRe   = regexp.MustCompile(`(?i)\b\d{1,2}\s(января|февраля|марта|апреля|мая|июня|июля|августа|сентября|октября|ноября|декабря)|\b(20\d\d)\b|\bq[1-4]\b|\d-?й квартал`)
	quotedRe     = regexp.MustCompile(`«([^»]{2,60})»`)
)

// Известные участники (регуляторы, ведомства) — для выделения «кто».
var knownEntities = []string{"Банк России", "ЦБ", "Минфин", "Минэкономразвития", "Минпромторг", "Правительство",
	"Госдума", "ФНС", "ФАС", "Росстат", "ОПЕК+", "РЖД", "Мосбиржа", "Минсельхоз", "Минэнерго", "ФТС", "Альфа-Банк"}

// Набор стемов крупных регионов/городов для привязки новости к региону компании.
func regionStem(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, w := range []string{"г.", "город ", "республика ", "область", "край", "автономный округ", "обл."} {
		name = strings.ReplaceAll(name, w, " ")
	}
	name = strings.TrimSpace(name)
	if f := strings.Fields(name); len(f) > 0 {
		name = f[0]
	}
	if runeLen(name) > 6 {
		name = truncRunes(name, runeLen(name)-2) // «московская» -> «москов»
	}
	return name
}

// directionFor — направление с учётом особенностей фактора.
func directionFor(factor, sentence string) string {
	dir := detectDirection(sentence)
	hl := strings.ToLower(sentence)
	switch factor {
	case "fx_rub": // «укрепление рубля» = up, «ослабление» = down (по рублю, а не по валюте)
		switch {
		case strings.Contains(hl, "укрепл") || strings.Contains(hl, "укрепи"):
			return "up"
		case strings.Contains(hl, "ослаб") || strings.Contains(hl, "девальвац"):
			return "down"
		}
		if strings.Contains(hl, "рубл") && (strings.Contains(hl, "доллар") || strings.Contains(hl, "юан")) {
			// «юань подорожал до …» = рубль ослаб
			if dir == "up" {
				return "down"
			} else if dir == "down" {
				return "up"
			}
		}
	case "agri": // «пошлина на пшеницу снижена» — меняется пошлина, а не цена зерна
		if strings.Contains(hl, "пошлин") && !strings.Contains(hl, "цен") {
			return "unknown"
		}
	case "labor": // «безработица снизилась / на минимуме», «дефицит кадров» = рынок труда перегрет (up)
		if strings.Contains(hl, "дефицит") || (strings.Contains(hl, "безработиц") && (strings.Contains(hl, "минимум") || dir == "down")) {
			return "up"
		}
	}
	return dir
}

// ClassifyEvent заполняет EventType, Factors, Sectors, Entities, KeyFacts, Suspicious.
func ClassifyEvent(e *Event, regionHints []string) {
	text := e.Content()
	low := strings.ToLower(text)
	sentences := splitSentences(text)

	// Факторы: ищем предложение с ключевым словом и по нему определяем направление.
	e.Factors = nil
	for _, f := range factors {
		// Берём первое предложение с ключевым словом, в котором понятно направление изменения;
		// если такого нет — первое предложение с ключевым словом.
		var hit string
		for _, s := range sentences {
			if containsAny(s, f.Keywords) {
				if hit == "" {
					hit = s
				}
				if directionFor(f.Key, s) != "unknown" {
					hit = s
					break
				}
			}
		}
		if hit == "" {
			continue
		}
		dir := directionFor(f.Key, hit)
		e.Factors = append(e.Factors, FactorSignal{Factor: f.Key, Name: f.Name, Direction: dir, Evidence: shorten(hit, 220)})
	}

	// Отрасли.
	e.Sectors = nil
	for _, s := range sectors {
		if len(s.Keywords) > 0 && containsAny(low, s.Keywords) {
			e.Sectors = append(e.Sectors, s.Key)
		}
	}

	// Тип события по самому «сильному» фактору.
	keys := e.FactorKeys()
	switch {
	case contains(keys, "key_rate"):
		e.EventType = "monetary_policy"
	case contains(keys, "fx_rub"):
		e.EventType = "fx"
	case contains(keys, "taxes") || contains(keys, "regulation"):
		e.EventType = "regulation"
	case contains(keys, "sanctions_trade"):
		e.EventType = "trade"
	case contains(keys, "oil") || contains(keys, "gas") || contains(keys, "metals") || contains(keys, "gold") || contains(keys, "agri"):
		e.EventType = "commodities"
	case contains(keys, "inflation") || contains(keys, "gdp_growth") || contains(keys, "consumer_demand") || contains(keys, "labor"):
		e.EventType = "macro"
	case len(e.Sectors) > 0:
		e.EventType = "sector"
	default:
		e.EventType = "other"
	}

	// Участники.
	var ents []string
	for _, k := range knownEntities {
		if strings.Contains(text, k) {
			ents = append(ents, k)
		}
	}
	for _, m := range quotedRe.FindAllStringSubmatch(text, -1) {
		ents = append(ents, m[1])
	}
	e.Entities = uniqueStrings(ents)

	// Регионы.
	e.Regions = nil
	for _, r := range regionHints {
		if st := regionStem(r); runeLen(st) >= 4 && strings.Contains(low, st) {
			e.Regions = append(e.Regions, r)
		}
	}
	e.Regions = uniqueStrings(e.Regions)

	// Ключевые факты — предложения с числами и датами (максимум 4).
	e.KeyFacts = nil
	bodySentences := splitSentences(e.Text) // заголовок в факты не дублируем
	if len(bodySentences) == 0 {
		bodySentences = sentences
	}
	for _, s := range bodySentences {
		if numberFactRe.MatchString(s) || dateFactRe.MatchString(s) {
			e.KeyFacts = append(e.KeyFacts, shorten(s, 260))
		}
		if len(e.KeyFacts) >= 4 {
			break
		}
	}
	if len(e.KeyFacts) == 0 && len(sentences) > 0 {
		e.KeyFacts = []string{shorten(sentences[0], 260)}
	}

	e.Suspicious = looksLikeInjection(text)
	sort.Strings(e.Sectors)
}
