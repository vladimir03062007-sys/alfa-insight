// Конфигурация времени выполнения.
// Секреты и параметры читаются из .env / переменных окружения,
// бизнес-правила (веса ранжирования, роли, источники) лежат в configs/*.json.
package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config — все настройки приложения.
type Config struct {
	Root string // корень проекта (там, где лежат configs/, data/, web/)

	// ---- LLM (OpenAI-совместимый API организаторов) ----
	LLMBaseURL         string // например https://<host>/v1
	LLMAPIKey          string // общий ключ команды (выдают организаторы)
	LLMModelFast       string // Gemma 4 26B A4B — классификация, извлечение фактов, самопроверка
	LLMModelReasoning  string // GLM-5.3-Flash — агентное рассуждение и интерпретация
	LLMModelGeneral    string // Qwen3.6 35B A3B — ответы на уточняющие вопросы
	LLMModelResearch   string // DeepSeek-V4.1-Flash — поиск взаимосвязей, анализ больших объёмов
	LLMModelText       string // Kimi-K2.6 — обработка и анализ текста (критик, резерв писателя)
	LLMModelVision     string // Qwen3-VL-30B-A3B-Instruct — изображения (графики в PDF аналитики)
	LLMDailyBudgetRub  float64
	LLMPrices          map[string][2]float64 // модель -> цена ₽ за 1 млн токенов (вход, выход)
	VisionOCR          bool
	LLMAllowedModels   []string
	LLMDailyTokenLimit int     // дневной лимит токенов (защита бюджета 500 ₽)
	LLMTemperature     float64 // низкая температура = меньше фантазий
	LLMTimeoutSec      int
	LLMMaxAgentSteps   int  // максимум шагов агента (tool calls) на один инсайт
	LLMCache           bool // кэшировать ответы LLM на диске (экономия токенов)
	LLMMode            string

	// ---- эмбеддинги ----
	EmbeddingProvider string // tfidf (по умолчанию) | bge
	BGEModel          string
	BGEEndpoint       string
	BGEAPI            string // ollama | openai
	BGEBatchSize      int
	BGEMaxChars       int

	// ---- сбор новостей ----
	NewsMode       string // reference | live | file
	NewsFile       string
	RequestTimeout int
	MaxPerSource   int
	MaxTotal       int
	NewsHours      int
	UserAgent      string

	// ---- сервер ----
	Addr string

	// ---- пути ----
	ConfigDir    string
	CompanyDir   string // реальные CSV организаторов
	NewsDir      string
	AnalyticsDir string
	StateDir     string
	OutputDir    string
	TraceDir     string
	AuditLogPath string
}

// cfg — глобальная конфигурация, заполняется один раз при старте программы.
var cfg = loadConfig()

// findRoot определяет корень проекта:
// 1) переменная APP_ROOT; 2) текущая папка, если в ней есть configs/;
// 3) папка, где лежит исполняемый файл.
func findRoot() string {
	if r := strings.TrimSpace(os.Getenv("APP_ROOT")); r != "" {
		return r
	}
	if wd, err := os.Getwd(); err == nil {
		if _, err := os.Stat(filepath.Join(wd, "configs", "ranking.json")); err == nil {
			return wd
		}
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if _, err := os.Stat(filepath.Join(dir, "configs", "ranking.json")); err == nil {
			return dir
		}
	}
	wd, _ := os.Getwd()
	return wd
}

// loadDotEnv читает файл .env (KEY=VALUE) и не перезаписывает
// переменные, которые уже заданы в окружении.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		} else if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func getenvInt(key string, def int) int {
	if v, err := strconv.Atoi(getenv(key, "")); err == nil {
		return v
	}
	return def
}

func getenvFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(getenv(key, ""), 64); err == nil {
		return v
	}
	return def
}

func getenvBool(key string, def bool) bool {
	switch strings.ToLower(getenv(key, "")) {
	case "1", "true", "yes", "on", "да":
		return true
	case "0", "false", "no", "off", "нет":
		return false
	}
	return def
}

// defaultAllowedModels — фрагменты имён моделей из Приложения 3 кейса.
// Вызов любой другой модели блокируется кодом (см. llm.go → modelAllowed).
var defaultAllowedModels = []string{
	"gemma-4-26b", "gemma4-26b", "gemma-4-26b-a4b", // Gemma 4 26B A4B
	"glm-5.3", "glm5.3", // GLM-5.3-Flash
	"qwen3.6", "qwen-3.6", // Qwen3.6 35B A3B
	"deepseek-v4.1", "deepseek-v4-1", // DeepSeek-V4.1-Flash
	"qwen3-vl-30b",           // Qwen3-VL-30B-A3B-Instruct
	"kimi-k2.6", "kimi-k2-6", // Kimi-K2.6
}

func loadConfig() *Config {
	root := findRoot()
	loadDotEnv(filepath.Join(root, ".env"))

	allowed := defaultAllowedModels
	if v := getenv("LLM_ALLOWED_MODELS", ""); v != "" {
		allowed = nil
		for _, x := range strings.Split(v, ",") {
			if x = strings.TrimSpace(strings.ToLower(x)); x != "" {
				allowed = append(allowed, x)
			}
		}
	}

	c := &Config{
		Root: root,

		LLMBaseURL:         strings.TrimRight(getenv("LLM_BASE_URL", ""), "/"),
		LLMAPIKey:          getenv("LLM_API_KEY", ""),
		LLMModelFast:       getenv("LLM_MODEL_FAST", "gemma-4-26b-a4b"),
		LLMModelReasoning:  getenv("LLM_MODEL_REASONING", "glm-5.3-flash"),
		LLMModelGeneral:    getenv("LLM_MODEL_GENERAL", "qwen3.6-35b-a3b"),
		LLMModelResearch:   getenv("LLM_MODEL_RESEARCH", "deepseek-v4.1-flash"),
		LLMModelText:       getenv("LLM_MODEL_TEXT", "kimi-k2.6"),
		LLMModelVision:     getenv("LLM_MODEL_VISION", "qwen3-vl-30b-a3b-instruct"),
		LLMDailyBudgetRub:  getenvFloat("LLM_DAILY_BUDGET_RUB", 150),
		VisionOCR:          getenvBool("ANALYTICS_VISION_OCR", false),
		LLMAllowedModels:   allowed,
		LLMDailyTokenLimit: getenvInt("LLM_DAILY_TOKEN_LIMIT", 400000),
		LLMTemperature:     getenvFloat("LLM_TEMPERATURE", 0.1),
		LLMTimeoutSec:      getenvInt("LLM_TIMEOUT", 120),
		LLMMaxAgentSteps:   getenvInt("LLM_MAX_AGENT_STEPS", 6),
		LLMCache:           getenvBool("LLM_CACHE", true),
		LLMMode:            strings.ToLower(getenv("LLM_MODE", "auto")),

		EmbeddingProvider: strings.ToLower(getenv("EMBEDDING_PROVIDER", "tfidf")),
		BGEModel:          getenv("BGE_MODEL", "bge-m3"),
		BGEEndpoint:       strings.TrimRight(getenv("BGE_ENDPOINT", "http://localhost:11434"), "/"),
		BGEAPI:            strings.ToLower(getenv("BGE_API", "ollama")),
		BGEBatchSize:      getenvInt("BGE_BATCH_SIZE", 8),
		BGEMaxChars:       getenvInt("BGE_MAX_CHARS", 6000),

		NewsMode:       strings.ToLower(getenv("NEWS_MODE", "reference")),
		NewsFile:       getenv("NEWS_FILE", ""),
		RequestTimeout: getenvInt("REQUEST_TIMEOUT", 20),
		MaxPerSource:   getenvInt("MAX_PER_SOURCE", 25),
		MaxTotal:       getenvInt("MAX_TOTAL", 400),
		NewsHours:      getenvInt("NEWS_HOURS", 72),
		UserAgent:      getenv("USER_AGENT", "AlfaInsight/1.0 (hackathon prototype)"),

		Addr: getenv("ADDR", ":8080"),

		ConfigDir:    filepath.Join(root, "configs"),
		CompanyDir:   getenv("COMPANY_DATA_DIR", filepath.Join(root, "data", "company")),
		NewsDir:      filepath.Join(root, "data", "news"),
		AnalyticsDir: getenv("ALFA_ANALYTICS_DIR", filepath.Join(root, "data", "alfa_analytics")),
		StateDir:     filepath.Join(root, "data", "state"),
		OutputDir:    filepath.Join(root, "output"),
	}
	c.TraceDir = filepath.Join(c.OutputDir, "traces")
	// Цены моделей (₽ за 1 млн токенов): LLM_PRICE_FAST="вход,выход" и т.д.
	c.LLMPrices = map[string][2]float64{}
	for env, model := range map[string]string{"FAST": c.LLMModelFast, "REASONING": c.LLMModelReasoning,
		"GENERAL": c.LLMModelGeneral, "RESEARCH": c.LLMModelResearch, "TEXT": c.LLMModelText, "VISION": c.LLMModelVision} {
		in, out, ok := strings.Cut(getenv("LLM_PRICE_"+env, ""), ",")
		if !ok {
			continue
		}
		pi, ok1 := parseNumber(in)
		po, ok2 := parseNumber(out)
		if ok1 && ok2 {
			c.LLMPrices[model] = [2]float64{pi, po}
		}
	}
	c.AuditLogPath = filepath.Join(c.OutputDir, "audit.log")
	if c.BGEBatchSize < 1 {
		c.BGEBatchSize = 8
	}
	if c.LLMMaxAgentSteps < 2 {
		c.LLMMaxAgentSteps = 2
	}
	for _, p := range []string{c.StateDir, c.OutputDir, c.TraceDir} {
		_ = os.MkdirAll(p, 0o755)
	}
	return c
}

// LLMEnabled — есть ли ключ и адрес API для LLM.
func (c *Config) LLMEnabled() bool {
	if c.LLMMode == "offline" {
		return false
	}
	return c.LLMBaseURL != "" && c.LLMAPIKey != ""
}
