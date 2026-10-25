// Тесты на реальных материалах банка (data/alfa_analytics/_text) и новых функциях:
// морфология, извлечение прогнозов и их пересмотров, актуальность позиции банка, калькулятор.
package main

import (
	"strings"
	"testing"
)

func TestMorphologyMatching(t *testing.T) {
	cases := []struct {
		text, term string
		want       bool
	}{
		{"Что думает банк о курсе рубля?", "курс рубл", true},
		{"решение по ключевой ставке", "ключевую ставк", true},
		{"курс российского рубля", "курс рубл", true},         // одно слово между частями термина
		{"для аккредитованных компаний", "кредитован", false}, // начало слова, а не середина
		{"медицинские услуги", "меди ", false},                // точное слово
		{"цены на медь и меди", "меди ", true},
		{"денежно-кредитной политики", "денежно-кредитн", true},
	}
	for _, c := range cases {
		if got := containsAny(c.text, []string{c.term}); got != c.want {
			t.Errorf("containsAny(%q, %q) = %v, ожидалось %v", c.text, c.term, got, c.want)
		}
	}
	if len(splitSentences("На конец 2026 г. ожидаем ставку 13,5%. Новое предложение.")) != 2 {
		t.Error("точка после «г.» не должна разрывать предложение")
	}
}

func TestQuestionFactors(t *testing.T) {
	for q, f := range map[string]string{
		"Что думает банк о курсе рубля?": "fx_rub",
		"Как ставка повлияет на долг?":   "key_rate",
		"Что будет с инфляцией?":         "inflation",
	} {
		found := false
		for _, s := range QuestionFactors(q) {
			found = found || s.Factor == f
		}
		if !found {
			t.Errorf("вопрос %q: фактор %s не найден", q, f)
		}
	}
}

func TestRealAnalyticsAndForecasts(t *testing.T) {
	app := newTestApp(t)
	if len(app.KB.Docs) < 9 || app.KB.Docs[0].Synthetic {
		t.Fatalf("ожидались 9 реальных материалов банка, загружено %d", len(app.KB.Docs))
	}
	stale := 0
	for _, d := range app.KB.Docs {
		if d.Stale {
			stale++
			if !strings.Contains(d.Date, "2021") {
				t.Errorf("устаревшим помечен свежий материал %s (%s)", d.Title, d.Date)
			}
		}
	}
	if stale != 1 {
		t.Errorf("устаревших материалов %d, ожидался 1 (сталь, 2021)", stale)
	}
	// Ключевая ставка на конец 2026: 13,5% в июле–начале сентября → 14,0% после заседания 11 сентября.
	var kr *ForecastTrack
	for _, tr := range app.KB.Tracks("key_rate") {
		if tr.Period == "2026" {
			tr := tr
			kr = &tr
		}
	}
	if kr == nil || kr.Current.Value != 14.0 || !kr.Revised || kr.Direction != "up" || kr.History[0].Value != 13.5 {
		t.Fatalf("прогноз ставки на конец 2026 извлечён неверно: %+v", kr)
	}
	if kr.Current.DocDate != "2026-09-11" {
		t.Errorf("актуальный прогноз должен быть из материала 11.09, а не %s", kr.Current.DocDate)
	}
	// Календарь: заседания 23 октября и 18 декабря из Бонд Брифа.
	cal := app.KB.Calendar()
	got := map[string]bool{}
	for _, c := range cal {
		got[c.Date] = true
	}
	if !got["2026-10-23"] || !got["2026-12-18"] {
		t.Errorf("в календаре нет заседаний 23.10 и 18.12: %+v", cal)
	}
}

func TestInsightUsesFreshBankForecast(t *testing.T) {
	app := newTestApp(t)
	e := findEvent(t, app, "сохранил ключевую ставку на уровне 14%")
	in, _, err := app.GenerateInsight(coGrain, "cfo", e.EventID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !in.SelfCheck.Passed {
		t.Fatalf("самопроверка не пройдена: %v", in.SelfCheck.Issues)
	}
	if !strings.Contains(in.BankView.Summary, "14,0%") || strings.Contains(in.BankView.Summary, "13,5%") {
		t.Errorf("позиция банка должна опираться на актуальный прогноз 14,0%%: %s", in.BankView.Summary)
	}
	if len(in.BankView.Forecasts) == 0 {
		t.Error("в позиции банка нет блока прогнозов")
	}
	for _, c := range in.BankView.Citations {
		if c.Page == 0 || c.File == "" {
			t.Errorf("цитата %s без файла/страницы — её нельзя проверить по первоисточнику", c.ID)
		}
	}
	// Каналы влияния: кредиты — риск, депозиты — возможность.
	if len(in.Consequences.Risks) == 0 || len(in.Consequences.Opportunities) == 0 {
		t.Error("для заёмщика с депозитами ожидаются и риск, и возможность")
	}
	if in.Scenario == nil {
		t.Fatal("для события о ставке ожидается калькулятор «что если»")
	}
}

func TestScenarioMath(t *testing.T) {
	app := newTestApp(t)
	p, _ := app.Store.Profile(coGrain)
	pack := BuildScenarios(p, app.KB, map[string]float64{"debt": 20, "floating": 50})
	for _, r := range pack.Results {
		if r.ID == "rate_shock" && r.DeltaRub != -0.1e9 { // 20 млрд × 50% × 1 п.п. = 0,1 млрд
			t.Errorf("стресс +1 п.п.: %v, ожидалось −0,1 млрд", r.DeltaRub)
		}
		if r.ID == "rate_forecast" && r.DeltaRub != 0.2e9 { // 20 × 50% × (14 − 12)% = 0,2 млрд
			t.Errorf("снижение 14→12%%: %v, ожидалось +0,2 млрд", r.DeltaRub)
		}
	}
}

func TestAskUnderstandsInflectedQuestion(t *testing.T) {
	app := newTestApp(t)
	ans, err := app.Ask(coGrain, "cfo", "", "Что думает банк о курсе рубля?")
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, s := range ans.Sections {
		joined += s.Title + ": " + strings.Join(s.Items, " ") + "\n"
	}
	if strings.Contains(joined, "нет информации") || !strings.Contains(joined, "руб./$") {
		t.Errorf("ответ на вопрос о курсе не содержит позиции банка:\n%s", joined)
	}
}
