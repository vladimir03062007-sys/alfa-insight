// alfa-insight: сервис персонализированной новостной аналитики для руководителей
// крупного бизнеса (кейс «Превратить поток новостей в бизнес-решения»).
//
// Команды:
//
//	serve       веб-интерфейс + REST API (по умолчанию http://localhost:8080)
//	companies   список компаний из данных
//	profile     бизнес-контекст компании
//	feed        персонализированная лента для компании и роли
//	insight     персонализированная интерпретация события (агент)
//	ask         уточняющий вопрос к аналитике
//	eval        проверка качества на отложенных (ранее не показанных) новостях
//	demo        консольная демонстрация: как меняется лента под разных клиентов
//	llm-check   проверить доступ к LLM по ключу организаторов
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
)

const usage = `alfa-insight — персонализированная новостная аналитика для крупного бизнеса

Использование:
  alfa-insight [--verbose] <команда> [флаги]

Команды:
  serve       Запустить веб-интерфейс и API (--addr :8080 --news reference|live|file)
  companies   Список компаний (--large — только выручка > 15 млрд ₽)
  profile     Бизнес-контекст компании (--company ID)
  feed        Лента для компании и роли (--company ID --role cfo)
  insight     Интерпретация события (--company ID --role cfo --event EVT-...)
  ask         Уточняющий вопрос (--company ID --role cfo --question "...")
  eval        Проверка на отложенных новостях data/news/holdout_news.json
  analytics   Материалы банка, их актуальность и извлечённые прогнозы
  demo        Демонстрация в консоли
  llm-models  Показать модели, доступные по ключу (GET /models)
  llm-check   Проверить подключение к LLM

Роли: owner, ceo, cfo, chief_accountant, treasury, clevel, employee
Флаги команды: alfa-insight <команда> --help
`

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "Ошибка: "+format+"\n", args...)
	os.Exit(1)
}

func printJSON(v any) {
	data, err := marshalPretty(v)
	if err != nil {
		fail("%v", err)
	}
	fmt.Println(string(data))
}

type common struct {
	news, newsFile *string
}

func newCmd(name string) (*flag.FlagSet, common) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.BoolVar(&verbose, "verbose", verbose, "подробный лог")
	c := common{
		news:     fs.String("news", cfg.NewsMode, "источник новостей: reference | live | file | holdout"),
		newsFile: fs.String("news-file", cfg.NewsFile, "JSON с новостями для --news file"),
	}
	return fs, c
}

func mustApp(c common) *App {
	app, err := NewApp(*c.news, *c.newsFile)
	if err != nil {
		fail("%v", err)
	}
	return app
}

func main() {
	global := flag.NewFlagSet("alfa-insight", flag.ExitOnError)
	global.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	global.BoolVar(&verbose, "verbose", false, "подробный лог")
	_ = global.Parse(os.Args[1:])
	if global.NArg() == 0 {
		global.Usage()
		os.Exit(2)
	}
	cmd, args := global.Arg(0), global.Args()[1:]

	switch cmd {
	case "serve":
		fs, c := newCmd("serve")
		addr := fs.String("addr", cfg.Addr, "адрес HTTP-сервера")
		_ = fs.Parse(args)
		app := mustApp(c)
		mode := "офлайн (без LLM: детерминированный агент)"
		if cfg.LLMEnabled() {
			mode = "LLM: " + cfg.LLMBaseURL
		}
		host := *addr
		if strings.HasPrefix(host, ":") {
			host = "localhost" + host
		}
		fmt.Printf("alfa-insight запущен: http://%s\nРежим: %s\nКомпаний: %d, событий в ленте: %d, материалов банка: %d\n",
			host, mode, len(app.Store.Companies), len(app.News.Events), len(app.KB.Docs))
		if err := http.ListenAndServe(*addr, app.Routes()); err != nil {
			fail("%v", err)
		}

	case "companies":
		fs, c := newCmd("companies")
		large := fs.Bool("large", false, "только крупный бизнес (выручка > 15 млрд ₽)")
		limit := fs.Int("limit", 30, "сколько показать")
		_ = fs.Parse(args)
		app := mustApp(c)
		for _, s := range app.Store.List(*large, *limit) {
			fmt.Printf("%-14s %-42s %-16s %s\n", s.ID, truncRunes(s.SectorName, 42), s.Revenue, s.Region)
		}

	case "profile":
		fs, c := newCmd("profile")
		company := fs.String("company", "", "ID компании (SYNTHETIC_U)")
		_ = fs.Parse(args)
		app := mustApp(c)
		p, err := app.Store.Profile(*company)
		if err != nil {
			fail("%v", err)
		}
		printJSON(p)

	case "feed":
		fs, c := newCmd("feed")
		company := fs.String("company", "", "ID компании")
		role := fs.String("role", "cfo", "роль пользователя")
		all := fs.Bool("all", false, "показать и отфильтрованные (нерелевантные) события")
		_ = fs.Parse(args)
		app := mustApp(c)
		feed, p, err := app.Feed(*company, *role, *all)
		if err != nil {
			fail("%v", err)
		}
		fmt.Printf("Лента для %s (%s), роль: %s\n\n", p.Company.ID, p.SectorName, getRole(*role).Name)
		for i, r := range feed {
			mark := " "
			if !r.Shown {
				mark = "×"
			}
			fmt.Printf("%s%2d. [%.3f] %-12s %s  (%s)\n", mark, i+1, r.Score, impactLabel[r.Impact], shorten(r.Event.Title, 80), r.Event.EventID)
			for _, why := range r.Reasons[:min(2, len(r.Reasons))] {
				fmt.Printf("      — %s\n", shorten(why, 110))
			}
		}

	case "insight":
		fs, c := newCmd("insight")
		company := fs.String("company", "", "ID компании")
		role := fs.String("role", "cfo", "роль пользователя")
		event := fs.String("event", "", "ID события (если пусто — первое в ленте)")
		trace := fs.Bool("trace", false, "напечатать трассировку агента")
		_ = fs.Parse(args)
		app := mustApp(c)
		if *event == "" {
			feed, _, err := app.Feed(*company, *role, false)
			if err != nil || len(feed) == 0 {
				fail("в ленте нет событий: %v", err)
			}
			*event = feed[0].Event.EventID
		}
		in, tr, err := app.GenerateInsight(*company, *role, *event, true)
		if err != nil {
			fail("%v", err)
		}
		printJSON(in)
		if *trace {
			printJSON(tr)
		}

	case "ask":
		fs, c := newCmd("ask")
		company := fs.String("company", "", "ID компании")
		role := fs.String("role", "cfo", "роль пользователя")
		event := fs.String("event", "", "ID события (необязательно)")
		q := fs.String("question", "", "вопрос")
		_ = fs.Parse(args)
		app := mustApp(c)
		ans, err := app.Ask(*company, *role, *event, *q)
		if err != nil {
			fail("%v", err)
		}
		printJSON(ans)

	case "eval":
		fs, c := newCmd("eval")
		n := fs.Int("companies", 5, "сколько компаний проверить")
		useLLM := fs.Bool("llm", false, "прогнать через LLM (тратит общий бюджет команды!)")
		_ = fs.Parse(args)
		if !*useLLM {
			cfg.LLMMode = "offline" // массовый прогон по умолчанию бесплатный
		}
		if *c.news == cfg.NewsMode && cfg.NewsMode == "reference" {
			*c.news = "holdout"
		}
		runEval(mustApp(c), *n)

	case "analytics":
		_, c := newCmd("analytics")
		_ = c
		vec, err := GetBackend(cfg.EmbeddingProvider)
		if err != nil {
			fail("%v", err)
		}
		kb, err := LoadKnowledgeBase(vec)
		if err != nil {
			fail("%v", err)
		}
		fmt.Println("Материалы банка:")
		for _, d := range kb.Docs {
			st := ""
			if d.Stale {
				st = fmt.Sprintf("  ⚠ устарел (%d дн.)", d.AgeDays)
			}
			fmt.Printf("  %-4s %-10s %-10s стр.%-3d %s%s\n", d.ID, d.Date, d.TextSource, d.Pages, shorten(d.Title, 80), st)
		}
		fmt.Println("\nПрогнозы банка и их пересмотры:")
		for _, t := range kb.Tracks("") {
			mark := " "
			if t.Revised {
				mark = "↻"
			}
			fmt.Printf(" %s %-38s %s: %s\n", mark, t.Name, t.Period, t.Summary)
		}

	case "demo":
		_, c := newCmd("demo")
		runDemo(mustApp(c))

	case "llm-check":
		if !cfg.LLMEnabled() {
			fail("LLM не настроена: заполните LLM_BASE_URL и LLM_API_KEY в .env")
		}
		fmt.Println("Проверка всех шести моделей (один короткий запрос к каждой, ~30 токенов):")
		for _, role := range modelRoles {
			model := modelForRole(role)
			if role == "vision" {
				fmt.Printf("· %-9s %-28s проверяется при чтении PDF (ANALYTICS_VISION_OCR=true)\n", role, model)
				continue
			}
			res, err := chatModel(model, []ChatMessage{{"user", "Ответь одним словом: работает?"}}, 10, 20, "llm-check")
			if err != nil {
				fmt.Printf("✗ %-9s %-28s %v\n", role, model, err)
				continue
			}
			fmt.Printf("✓ %-9s %-28s %q (токены %d+%d)\n", role, res.Model, shorten(res.Text, 40), res.PromptTokens, res.CompletionTokens)
		}
		u := usageSnapshot()
		fmt.Printf("\nСегодня этим экземпляром израсходовано: %d токенов, %.2f ₽ (если заданы цены LLM_PRICE_*)\n",
			u.PromptTokens+u.CompletionTokens, u.CostRub)

	case "llm-models":
		if cfg.LLMBaseURL == "" || cfg.LLMAPIKey == "" {
			fail("заполните LLM_BASE_URL и LLM_API_KEY в .env")
		}
		names, err := ListModels()
		if err != nil {
			fail("%v", err)
		}
		for _, n := range names {
			mark := "  (не из списка кейса — использовать нельзя)"
			if modelAllowed(n) {
				mark = "  ✓ разрешена кейсом"
			}
			fmt.Println(n + mark)
		}

	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "Неизвестная команда: %s\n\n%s", cmd, usage)
		os.Exit(2)
	}
}
