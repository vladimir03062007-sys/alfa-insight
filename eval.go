// Проверка качества (критерии 05 и 08 кейса): прогон агента на отложенных новостях,
// которых не было при разработке правил, и подсчёт метрик качества контента.
package main

import (
	"fmt"
	"sort"
	"strings"
)

type evalRow struct {
	Company, Event, Impact string
	Passed                 bool
	Bank                   bool
	Confidence             float64
	Issues                 []string
}

func runEval(app *App, nCompanies int) {
	companies := app.Store.List(false, nCompanies)
	fmt.Printf("Оценка на %d событиях (режим новостей: %s) × %d компаний, LLM: %v\n\n",
		len(app.News.Events), app.News.Mode, len(companies), cfg.LLMEnabled())
	var rows []evalRow
	shownTotal, hiddenTotal := 0, 0
	withForecast, withCalc, unflagged := 0, 0, 0
	feeds := map[string][]string{}
	for _, c := range companies {
		feed, _, err := app.Feed(c.ID, "cfo", true)
		if err != nil {
			fail("%v", err)
		}
		for _, r := range feed {
			if !r.Shown {
				hiddenTotal++
				continue
			}
			shownTotal++
			if len(feeds[c.ID]) < 3 {
				feeds[c.ID] = append(feeds[c.ID], r.Event.EventID)
			}
		}
		for _, id := range feeds[c.ID] {
			in, _, err := app.GenerateInsight(c.ID, "cfo", id, true)
			if err != nil {
				fail("%v", err)
			}
			rows = append(rows, evalRow{c.ID, id, in.Impact, in.SelfCheck.Passed, in.BankView.Available, in.Confidence, in.SelfCheck.Issues})
			if len(in.BankView.Forecasts) > 0 {
				withForecast++
			}
			if in.Scenario != nil {
				withCalc++
			}
			for _, ct := range in.BankView.Citations {
				if (ct.Stale || len(app.KB.StaleClaims([]string{ct.ID})) > 0) && ct.Note == "" {
					unflagged++
				}
			}
		}
	}
	passed, bank := 0, 0
	conf := 0.0
	for _, r := range rows {
		mark := "✓"
		if !r.Passed {
			mark = "✗"
			passed--
		}
		passed++
		if r.Bank {
			bank++
		}
		conf += r.Confidence
		fmt.Printf("%s %-12s %-16s %-12s conf=%.2f bank=%v %s\n", mark, r.Company, r.Event, r.Impact, r.Confidence, r.Bank,
			strings.Join(r.Issues, "; "))
	}
	// Персонализация: насколько различаются топ-3 ленты разных компаний.
	overlap, pairs := 0.0, 0
	ids := sortedKeys(feeds)
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			a, b := feeds[ids[i]], feeds[ids[j]]
			inter := 0
			for _, x := range a {
				if contains(b, x) {
					inter++
				}
			}
			if n := max(len(a), len(b)); n > 0 {
				overlap += float64(inter) / float64(n)
				pairs++
			}
		}
	}
	fmt.Println("\nМетрики:")
	if len(rows) > 0 {
		fmt.Printf("  инсайтов: %d, самопроверка пройдена: %.0f%%, с позицией банка: %.0f%%, средняя уверенность: %.2f\n",
			len(rows), 100*float64(passed)/float64(len(rows)), 100*float64(bank)/float64(len(rows)), conf/float64(len(rows)))
	}
	fmt.Printf("  показано событий: %d, отфильтровано как нерелевантные: %d\n", shownTotal, hiddenTotal)
	if pairs > 0 {
		fmt.Printf("  среднее пересечение топ-3 лент разных компаний: %.0f%% (меньше — сильнее персонализация)\n", 100*overlap/float64(pairs))
	}
	if len(rows) > 0 {
		fmt.Printf("  с актуальным числовым прогнозом банка: %.0f%%, с калькулятором в рублях: %.0f%%, цитат устаревших/пересмотренных материалов без пометки: %d\n",
			100*float64(withForecast)/float64(len(rows)), 100*float64(withCalc)/float64(len(rows)), unflagged)
	}
	// Устойчивость разметки к формулировкам: падежи, порядок слов, разговорные вопросы.
	ok := 0
	for _, c := range paraphraseSet {
		ev := &Event{Title: c.text}
		ClassifyEvent(ev, nil)
		got := contains(ev.FactorKeys(), c.factor)
		if !got && c.question {
			for _, f := range QuestionFactors(c.text) {
				got = got || f.Factor == c.factor
			}
		}
		if got {
			ok++
		} else {
			fmt.Printf("  ✗ не распознан фактор %s: %q\n", c.factor, c.text)
		}
	}
	fmt.Printf("  распознавание факторов на перефразированных формулировках: %d из %d\n", ok, len(paraphraseSet))
}

// paraphraseSet — формулировки, которых нет в таксономии дословно (другие падежи и порядок слов).
var paraphraseSet = []struct {
	text, factor string
	question     bool
}{
	{"Регулятор оставил ключевую ставку без изменений", "key_rate", false},
	{"ЦБ может вернуться к повышению ключевой ставки", "key_rate", false},
	{"Решение по ключевой ставке перенесено на декабрь", "key_rate", false},
	{"Курс российского рубля к доллару снизился", "fx_rub", false},
	{"Рубль ослаб к юаню на Мосбирже", "fx_rub", false},
	{"Инфляционные ожидания населения выросли", "inflation", false},
	{"Потребительские цены за неделю выросли на 0,1%", "inflation", false},
	{"Котировки нефти марки Brent упали", "oil", false},
	{"Стоимость металлопроката на внутреннем рынке выросла", "metals", false},
	{"Оборот розничной торговли замедлился", "consumer_demand", false},
	{"Безработица осталась на историческом минимуме", "labor", false},
	{"Тарифы на грузовые перевозки РЖД проиндексируют", "logistics", false},
	{"Ставки по ипотеке снова растут", "construction_realty", false},
	{"Что будет с курсом?", "fx_rub", true},
	{"Как ставка повлияет на наш долг?", "key_rate", true},
	{"Чего ждать от цен в следующем году?", "inflation", true},
}

func runDemo(app *App) {
	list := app.Store.List(true, 50)
	// Берём компании из разных отраслей.
	seen := map[string]bool{}
	var picked []CompanySummary
	for _, c := range list {
		if !seen[c.Sector] {
			seen[c.Sector] = true
			picked = append(picked, c)
		}
		if len(picked) == 3 {
			break
		}
	}
	sort.SliceStable(picked, func(i, j int) bool { return picked[i].ID < picked[j].ID })
	for _, c := range picked {
		feed, p, err := app.Feed(c.ID, "cfo", false)
		if err != nil {
			fail("%v", err)
		}
		fmt.Printf("\n=== %s — %s, %s ===\n", c.ID, p.SectorName, p.Scale)
		for i, r := range feed {
			if i >= 4 {
				break
			}
			fmt.Printf("  %d. [%.2f] %-22s %s\n", i+1, r.Score, impactLabel[r.Impact], shorten(r.Event.Title, 85))
		}
		if len(feed) > 0 {
			in, _, _ := app.GenerateInsight(c.ID, "cfo", feed[0].Event.EventID, false)
			fmt.Printf("\n  ▶ %s\n  Почему важно: %s\n", in.Headline, shorten(in.WhyItMatters.Text, 400))
			if in.BankView.Available {
				fmt.Printf("  Позиция банка: %s\n", shorten(in.BankView.Summary, 300))
			}
			for i, a := range in.Actions {
				if i >= 2 {
					break
				}
				fmt.Printf("  → %s\n", a.Text)
			}
			fmt.Printf("  Самопроверка: %v (итераций %d), трассировка: %s\n", in.SelfCheck.Passed, in.SelfCheck.Iterations, in.TraceID)
		}
	}
}
