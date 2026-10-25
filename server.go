// HTTP-сервер: REST API (описание — docs/openapi.yaml) и веб-интерфейс прототипа.
package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed web/*
var webFS embed.FS

func writeAPI(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	data, _ := marshalPretty(v)
	w.Write(data)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	writeAPI(w, status, map[string]string{"error": msg})
}

// withLimits — ограничение размера тела запроса и простое логирование.
func withLimits(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		start := time.Now()
		next.ServeHTTP(w, r)
		logInfo("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func (a *App) companyRole(r *http.Request) (string, string, bool) {
	c := r.URL.Query().Get("company")
	role := r.URL.Query().Get("role")
	if role == "" {
		role = "cfo"
	}
	_, ok := a.Store.Companies[c]
	return c, role, ok
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeAPI(w, 200, map[string]any{"status": "ok", "llm_enabled": cfg.LLMEnabled(), "news_mode": a.News.Mode,
			"events": len(a.News.Events), "companies": len(a.Store.Companies), "synthetic_company_data": a.Store.Synthetic,
			"analytics_docs": len(a.KB.Docs), "analytics_synthetic": len(a.KB.Docs) > 0 && a.KB.Docs[0].Synthetic,
			"synthetic_news": a.News.Synthetic(), "embedding": a.Vec.Name()})
	})

	mux.HandleFunc("GET /api/roles", func(w http.ResponseWriter, r *http.Request) { writeAPI(w, 200, roles) })

	mux.HandleFunc("GET /api/companies", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 50
		}
		writeAPI(w, 200, a.Store.List(r.URL.Query().Get("large") == "1", limit))
	})

	mux.HandleFunc("GET /api/companies/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, err := a.Store.Profile(r.PathValue("id"))
		if err != nil {
			apiError(w, 404, err.Error())
			return
		}
		writeAPI(w, 200, p)
	})

	mux.HandleFunc("GET /api/feed", func(w http.ResponseWriter, r *http.Request) {
		c, role, ok := a.companyRole(r)
		if !ok {
			apiError(w, 400, "укажите существующую компанию: ?company=...&role=...")
			return
		}
		feed, p, err := a.Feed(c, role, r.URL.Query().Get("all") == "1")
		if err != nil {
			apiError(w, 500, err.Error())
			return
		}
		if f := r.URL.Query().Get("factor"); f != "" {
			var filtered []RankedEvent
			for _, x := range feed {
				for _, fs := range x.Event.Factors {
					if fs.Factor == f {
						filtered = append(filtered, x)
						break
					}
				}
			}
			feed = filtered
		}
		if feed == nil {
			feed = []RankedEvent{}
		}
		writeAPI(w, 200, map[string]any{"company": p.Company.ID, "role": getRole(role), "sector": p.SectorName,
			"total_events": len(a.News.Events), "items": feed})
	})

	mux.HandleFunc("GET /api/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		e, ok := a.News.Get(r.PathValue("id"))
		if !ok {
			apiError(w, 404, "событие не найдено")
			return
		}
		writeAPI(w, 200, e.View())
	})

	mux.HandleFunc("GET /api/insight", func(w http.ResponseWriter, r *http.Request) {
		c, role, ok := a.companyRole(r)
		if !ok {
			apiError(w, 400, "укажите существующую компанию")
			return
		}
		audit(AuditRecord{Kind: "api", Actor: role, Company: c, Name: "insight", Args: r.URL.Query().Get("event")})
		in, _, err := a.GenerateInsight(c, role, r.URL.Query().Get("event"), r.URL.Query().Get("force") == "1")
		if err != nil {
			apiError(w, 404, err.Error())
			return
		}
		writeAPI(w, 200, in)
	})

	mux.HandleFunc("POST /api/ask", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Company, Role, Event, Question string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apiError(w, 400, "некорректный JSON")
			return
		}
		ans, err := a.Ask(req.Company, req.Role, req.Event, req.Question)
		if err != nil {
			apiError(w, 400, err.Error())
			return
		}
		writeAPI(w, 200, ans)
	})

	mux.HandleFunc("POST /api/feedback", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Company, Role, Event, Vote, Comment string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apiError(w, 400, "некорректный JSON")
			return
		}
		if req.Vote != "like" && req.Vote != "dislike" {
			apiError(w, 400, "vote: like | dislike")
			return
		}
		e, ok := a.News.Get(req.Event)
		if !ok {
			apiError(w, 404, "событие не найдено")
			return
		}
		if err := NewFeedbackStore(req.Company, req.Role).Add(e, req.Vote, truncRunes(req.Comment, 500)); err != nil {
			apiError(w, 500, err.Error())
			return
		}
		audit(AuditRecord{Kind: "api", Actor: req.Role, Company: req.Company, Name: "feedback", Args: req.Event + " " + req.Vote})
		writeAPI(w, 200, map[string]string{"status": "saved"})
	})

	mux.HandleFunc("GET /api/trace/{id}", func(w http.ResponseWriter, r *http.Request) {
		t, ok := a.Trace(r.PathValue("id"))
		if !ok {
			apiError(w, 404, "трассировка не найдена")
			return
		}
		writeAPI(w, 200, t)
	})

	mux.HandleFunc("GET /api/analytics", func(w http.ResponseWriter, r *http.Request) {
		writeAPI(w, 200, a.KB.Docs)
	})

	// Прогнозы банка, извлечённые из материалов, и история их пересмотров.
	mux.HandleFunc("GET /api/forecasts", func(w http.ResponseWriter, r *http.Request) {
		writeAPI(w, 200, map[string]any{"tracks": a.KB.Tracks(r.URL.Query().Get("factor")), "calendar": a.KB.Calendar(), "docs": a.KB.Docs})
	})

	// Калькулятор «что если»: GET /api/scenario?company=U0001&debt=50&floating=67&export=30&import=0
	mux.HandleFunc("GET /api/scenario", func(w http.ResponseWriter, r *http.Request) {
		p, err := a.Store.Profile(r.URL.Query().Get("company"))
		if err != nil {
			writeAPI(w, 404, map[string]string{"error": err.Error()})
			return
		}
		ov := map[string]float64{}
		for _, k := range []string{"revenue", "debt", "floating", "export", "import"} {
			if v, ok := parseNumber(r.URL.Query().Get(k)); ok {
				ov[k] = v
			}
		}
		writeAPI(w, 200, BuildScenarios(p, a.KB, ov))
	})

	mux.HandleFunc("GET /api/metrics", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		m := a.Metrics
		a.mu.Unlock()
		out := map[string]any{"insights": m.Insights, "questions": m.Questions, "revisions": m.Revisions,
			"llm_fallbacks": m.LLMFallbacks, "llm_usage_today": usageSnapshot(), "llm_daily_token_limit": cfg.LLMDailyTokenLimit,
			"llm_daily_budget_rub": cfg.LLMDailyBudgetRub, "llm_prices_known": len(cfg.LLMPrices) > 0, "llm_routing": llmRouting()}
		if m.Insights > 0 {
			out["self_check_pass_rate"] = round(float64(m.SelfCheckPassed)/float64(m.Insights), 3)
			out["bank_view_coverage"] = round(float64(m.WithBankView)/float64(m.Insights), 3)
			out["avg_confidence"] = round(m.ConfidenceSum/float64(m.Insights), 3)
		}
		out["feedback"] = feedbackTotals()
		writeAPI(w, 200, out)
	})

	mux.HandleFunc("POST /api/refresh", func(w http.ResponseWriter, r *http.Request) {
		mode := r.URL.Query().Get("mode")
		if mode == "" {
			mode = a.News.Mode
		}
		if !contains([]string{"reference", "live", "holdout"}, mode) {
			apiError(w, 400, "mode: reference | live | holdout")
			return
		}
		if err := a.ReloadNews(mode, ""); err != nil {
			apiError(w, 502, err.Error())
			return
		}
		writeAPI(w, 200, map[string]any{"status": "ok", "mode": mode, "events": len(a.News.Events), "sources": a.News.Status})
	})
	return withLimits(mux)
}

// feedbackTotals — суммарная обратная связь по всем пользователям (для мониторинга качества).
func feedbackTotals() map[string]int {
	out := map[string]int{"like": 0, "dislike": 0}
	entries, _ := readDirSafe(cfg.StateDir + "/feedback")
	for _, name := range entries {
		var fs FeedbackStore
		if readJSON(cfg.StateDir+"/feedback/"+name, &fs) == nil {
			l, d := fs.Stats()
			out["like"] += l
			out["dislike"] += d
		}
	}
	return out
}

func readDirSafe(dir string) ([]string, error) {
	var out []string
	entries, err := osReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.HasSuffix(e, ".json") {
			out = append(out, e)
		}
	}
	return out, nil
}

// llmRouting — какая модель решает какую задачу (с резервными).
func llmRouting() map[string][]string {
	out := map[string][]string{}
	for task, chain := range taskChains {
		for _, r := range chain {
			out[task] = append(out[task], modelForRole(r))
		}
	}
	return out
}
