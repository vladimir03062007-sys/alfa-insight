// Данные о компаниях-клиентах (Приложение 1 кейса) и построение бизнес-контекста:
//   - df_Uid_data_hack.csv      — карточка юрлица;
//   - df_Fid_Uid_link_hack.csv  — связанные физлица (для связей между компаниями);
//   - df_Uid_payment_hack.csv   — платежи между компаниями (контрагенты).
//
// Из сырых полей строятся признаки компании и её «экспозиции» к факторам:
// какой фактор, в какую сторону и почему влияет на бизнес. Каждая экспозиция
// хранит поле-источник, чтобы вывод можно было оттрассировать до данных.
package main

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const largeBusinessRevenue = 15e9 // крупный бизнес — выручка свыше 15 млрд ₽

// Company — строка df_Uid_data_hack.csv.
type Company struct {
	ID           string          `json:"id"`
	OrgType      string          `json:"org_type"`
	OKFS         string          `json:"okfs_name"`
	Age          string          `json:"company_age"`
	EcoStage     string          `json:"eco_stage"`
	OKVEDCode    string          `json:"okved_code"`
	OKVEDName    string          `json:"okved_name"`
	NegativeMark string          `json:"negative_mark"`
	WorkersFNS   string          `json:"workers_fns"`
	WorkersSPARK string          `json:"workers_spark"`
	ClientType   string          `json:"client_type"`
	Products     map[string]bool `json:"products"` // RKO, GAR, LOA, LEA, DEP, ZPP, FACTORING
	FTSFlag      bool            `json:"fts_flag"`
	FTSCountry   string          `json:"fts_country"`
	Revenue      float64         `json:"revenue"`
	RevenueKnown bool            `json:"revenue_known"`
	GroupID      string          `json:"group_id"`
	City         string          `json:"city"`
	Region       string          `json:"region"`
	Sector       string          `json:"sector"`
}

// Counterparty — агрегированный контрагент по платежам.
type Counterparty struct {
	ID       string  `json:"id"`
	Role     string  `json:"role"` // supplier (компания платит) | customer (платят компании)
	Amount   float64 `json:"amount"`
	Count    float64 `json:"count"`
	Share    float64 `json:"share"` // доля в обороте этого направления
	Sector   string  `json:"sector"`
	SectorNm string  `json:"sector_name"`
	Region   string  `json:"region,omitempty"`
}

// Exposure — чувствительность конкретной компании к фактору.
type Exposure struct {
	Factor     string   `json:"factor"`
	FactorName string   `json:"factor_name"`
	Weight     float64  `json:"weight"` // 0..1 — насколько фактор важен компании
	Sign       float64  `json:"sign"`   // +1: рост фактора хорош; -1: плох; 0: неоднозначно
	Reasons    []string `json:"reasons"`
	Fields     []string `json:"fields"` // поля данных, на которых основан вывод
	EffectUp   string   `json:"effect_up"`
	EffectDown string   `json:"effect_down"`
	// Channels — отдельные каналы влияния (кредиты, депозиты, ВЭД, контрагенты…).
	// У каждого свой знак и своё следствие: ставка вредит заёмщику, но помогает вкладчику.
	Channels []Channel `json:"channels"`
}

// Channel — один канал влияния фактора на компанию с полем-источником.
type Channel struct {
	Reason     string  `json:"reason"`
	Field      string  `json:"field"`
	Sign       float64 `json:"sign"`
	Weight     float64 `json:"weight"`
	EffectUp   string  `json:"effect_up"`
	EffectDown string  `json:"effect_down"`
}

// Feature — признак компании с указанием поля-источника (для трассировки).
type Feature struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Field string `json:"field"`
}

// CompanyProfile — бизнес-контекст компании, который видит агент.
type CompanyProfile struct {
	Company         Company            `json:"company"`
	SectorName      string             `json:"sector_name"`
	Scale           string             `json:"scale"`
	IsLarge         bool               `json:"is_large"`
	Features        []Feature          `json:"features"`
	Exposures       []Exposure         `json:"exposures"`
	Suppliers       []Counterparty     `json:"top_suppliers"`
	Customers       []Counterparty     `json:"top_customers"`
	CounterSectors  map[string]float64 `json:"counterparty_sectors"` // отрасль -> доля оборота
	GroupMembers    []string           `json:"group_members"`
	LinkedPersons   int                `json:"linked_persons"`
	LinkedCompanies []string           `json:"linked_companies"` // через общих физлиц
	// PersonCities — города связанных физлиц (CITY_NAME), MacroRegions — их макрорегионы (RC_NAME).
	PersonCities map[string]int `json:"person_cities"`
	MacroRegions []string       `json:"macro_regions"`
	DataQuality  []string       `json:"data_quality"` // какие данные отсутствуют/неоднозначны
	Synthetic    bool           `json:"synthetic"`
}

// ProfileText — текстовое описание для семантического сопоставления.
func (p *CompanyProfile) ProfileText() string {
	var b strings.Builder
	b.WriteString(p.SectorName + ". " + p.Company.OKVEDName + ". ")
	for _, e := range p.Exposures {
		if e.Weight >= 0.4 {
			b.WriteString(e.FactorName + ". ")
		}
	}
	if s, ok := sectorIndex[p.Company.Sector]; ok {
		b.WriteString(strings.Join(s.Keywords, " "))
	}
	if p.Company.Region != "" {
		b.WriteString(". " + p.Company.Region)
	}
	return b.String()
}

func (p *CompanyProfile) Exposure(factor string) *Exposure {
	for i := range p.Exposures {
		if p.Exposures[i].Factor == factor {
			return &p.Exposures[i]
		}
	}
	return nil
}

// ---------- хранилище ----------

type paymentEdge struct {
	Other  string
	Amount float64
	Count  float64
}

type CompanyStore struct {
	Companies   map[string]*Company
	Order       []string
	outgoing    map[string][]paymentEdge // payer -> beneficiaries
	incoming    map[string][]paymentEdge // beneficiary -> payers
	personsOf   map[string][]string      // company -> persons
	companiesOf map[string][]string      // person -> companies
	groups      map[string][]string
	personGeo   map[string][2]string // физлицо -> (город CITY_NAME, регион RC_NAME)
	Synthetic   bool
	Dir         string
	mu          sync.Mutex
	cache       map[string]*CompanyProfile
}

// LoadCompanyStore читает CSV из data/company (реальные данные организаторов),
// а если их нет — из data/synthetic.
func LoadCompanyStore() (*CompanyStore, error) {
	dir, synthetic := cfg.CompanyDir, false
	if _, err := os.Stat(filepath.Join(dir, "df_Uid_data_hack.csv")); err != nil {
		return nil, fmt.Errorf("не найден %s: данные о компаниях должны лежать в %s", "df_Uid_data_hack.csv", dir)
	}
	s := &CompanyStore{
		Companies: map[string]*Company{}, outgoing: map[string][]paymentEdge{},
		incoming: map[string][]paymentEdge{}, personsOf: map[string][]string{},
		companiesOf: map[string][]string{}, groups: map[string][]string{}, personGeo: map[string][2]string{},
		Synthetic: synthetic, Dir: dir, cache: map[string]*CompanyProfile{},
	}
	if err := s.loadCompanies(filepath.Join(dir, "df_Uid_data_hack.csv")); err != nil {
		return nil, err
	}
	if err := s.loadLinks(filepath.Join(dir, "df_Fid_Uid_link_hack.csv")); err != nil {
		logWarn("связи физлиц не загружены: %v", err)
	}
	if err := s.loadPayments(filepath.Join(dir, "df_Uid_payment_hack.csv")); err != nil {
		logWarn("платежи не загружены: %v", err)
	}
	logInfo("Компаний: %d, источник: %s", len(s.Companies), dir)
	return s, nil
}

// readCSV — потоковое чтение CSV с автоопределением разделителя (, ; \t) и BOM.
func readCSV(path string, fn func(row map[string]string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	head, _ := br.Peek(4096)
	firstLine := string(head)
	if i := strings.IndexByte(firstLine, '\n'); i >= 0 {
		firstLine = firstLine[:i]
	}
	delim := ','
	best := strings.Count(firstLine, ",")
	for _, d := range []rune{';', '\t', '|'} {
		if n := strings.Count(firstLine, string(d)); n > best {
			best, delim = n, d
		}
	}
	r := csv.NewReader(br)
	r.Comma = delim
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.ReuseRecord = true
	header, err := r.Read()
	if err != nil {
		return err
	}
	cols := make([]string, len(header))
	for i, h := range header {
		h = strings.TrimPrefix(h, "\ufeff")
		cols[i] = strings.ToUpper(strings.TrimSpace(strings.Trim(h, `"`)))
	}
	// Одна карта на весь файл (значения перезаписываются): в файле платежей >1 млн строк,
	// отдельная карта на строку заметно замедляет загрузку. fn не должна сохранять row.
	row := make(map[string]string, len(cols))
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			logDebug("%s: пропущена строка: %v", path, err)
			continue
		}
		for i, c := range cols {
			if i < len(rec) {
				row[c] = strings.TrimSpace(rec[i])
			} else {
				row[c] = ""
			}
		}
		fn(row)
	}
	return nil
}

// parseFlag понимает 1/0, Y/N, true/false, да/нет.
func parseFlag(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "1.0", "y", "yes", "true", "t", "да", "д", "есть", "+":
		return true
	}
	if f, ok := parseNumber(v); ok && f > 0 {
		return true
	}
	return false
}

func cleanVal(v string) string {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "nan", "null", "none", "-", "":
		return ""
	}
	return v
}

var productFields = map[string]string{
	"RKO": "CL_PRODUCT_RKO_MARK", "GAR": "CL_PRODUCT_GAR_MARK", "LOA": "CL_PRODUCT_LOA_MARK",
	"LEA": "CL_PRODUCT_LEA_MARK", "DEP": "CL_PRODUCT_DEP_MARK", "ZPP": "CL_PRODUCT_ZPP_MARK",
	"FACTORING": "FACTORING_MARK",
}

var productNames = map[string]string{
	"RKO": "расчётно-кассовое обслуживание", "GAR": "банковские гарантии", "LOA": "кредитные продукты",
	"LEA": "лизинг", "DEP": "депозиты", "ZPP": "зарплатный проект", "FACTORING": "факторинг",
}

func (s *CompanyStore) loadCompanies(path string) error {
	return readCSV(path, func(row map[string]string) {
		id := cleanVal(row["SYNTHETIC_U"])
		if id == "" {
			return
		}
		c := &Company{
			ID: id, OrgType: cleanVal(row["ORG_TYPE"]), OKFS: cleanVal(row["OKFS_NAME"]),
			Age: cleanVal(row["COMPANY_AGE_YEARS_GRADATION"]), EcoStage: cleanVal(row["ECO_MARK_LABEL"]),
			OKVEDCode: cleanVal(row["MAINOKVED_CODE"]), OKVEDName: cleanVal(row["MAINOKVED_NAME"]),
			NegativeMark: cleanVal(row["NEGATIVEMARK"]), WorkersFNS: cleanVal(row["AMOUNT_WORKERS_FNS_GRADATION"]),
			WorkersSPARK: cleanVal(row["AMOUNT_WORKERS_SPARK_GRADATION"]), ClientType: cleanVal(row["CL_TYPE"]),
			Products: map[string]bool{}, FTSFlag: parseFlag(row["FTS_FLAG"]), FTSCountry: cleanVal(row["FTS_COUNTRY_NAME"]),
			GroupID: cleanVal(row["GROUP_ID"]), City: cleanVal(row["CITY"]), Region: cleanVal(row["REGION"]),
		}
		for p, field := range productFields {
			c.Products[p] = parseFlag(row[field])
		}
		c.Revenue, c.RevenueKnown = parseNumber(row["REVENUE_COMPANY"])
		c.Sector = sectorByOKVED(c.OKVEDCode)
		if _, ok := s.Companies[id]; !ok {
			s.Order = append(s.Order, id)
		}
		s.Companies[id] = c
		if c.GroupID != "" {
			s.groups[c.GroupID] = append(s.groups[c.GroupID], id)
		}
	})
}

func (s *CompanyStore) loadLinks(path string) error {
	return readCSV(path, func(row map[string]string) {
		f, u := cleanVal(row["SYNTHETIC_F"]), cleanVal(row["SYNTHETIC_U"])
		if f == "" || u == "" {
			return
		}
		s.personsOf[u] = append(s.personsOf[u], f)
		s.companiesOf[f] = append(s.companiesOf[f], u)
		if city, reg := cleanVal(row["CITY_NAME"]), cleanVal(row["RC_NAME"]); city != "" || reg != "" {
			s.personGeo[f] = [2]string{city, reg}
		}
	})
}

func (s *CompanyStore) loadPayments(path string) error {
	return readCSV(path, func(row map[string]string) {
		ben, payer := cleanVal(row["BENEFICIARY"]), cleanVal(row["PAYER"])
		if ben == "" || payer == "" || ben == payer {
			return
		}
		amt, _ := parseNumber(row["PAY_AMT"])
		cnt, _ := parseNumber(row["PAY_CNT"])
		s.outgoing[payer] = append(s.outgoing[payer], paymentEdge{ben, amt, cnt})
		s.incoming[ben] = append(s.incoming[ben], paymentEdge{payer, amt, cnt})
	})
}

// CompanySummary — краткая карточка для списка компаний.
type CompanySummary struct {
	ID         string   `json:"id"`
	Sector     string   `json:"sector"`
	SectorName string   `json:"sector_name"`
	OKVEDName  string   `json:"okved_name"`
	Revenue    string   `json:"revenue"`
	RevenueRaw float64  `json:"revenue_raw"`
	Region     string   `json:"region"`
	IsLarge    bool     `json:"is_large"`
	Products   []string `json:"products"`
}

func (s *CompanyStore) List(onlyLarge bool, limit int) []CompanySummary {
	var out []CompanySummary
	for _, id := range s.Order {
		c := s.Companies[id]
		large := c.RevenueKnown && c.Revenue >= largeBusinessRevenue
		if onlyLarge && !large {
			continue
		}
		var prods []string
		for _, p := range []string{"LOA", "DEP", "LEA", "GAR", "ZPP", "FACTORING", "RKO"} {
			if c.Products[p] {
				prods = append(prods, p)
			}
		}
		rev := "нет данных"
		if c.RevenueKnown {
			rev = formatRub(c.Revenue)
		}
		out = append(out, CompanySummary{ID: id, Sector: c.Sector, SectorName: sectorName(c.Sector),
			OKVEDName: c.OKVEDName, Revenue: rev, RevenueRaw: c.Revenue, Region: c.Region, IsLarge: large, Products: prods})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].RevenueRaw > out[j].RevenueRaw })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// aggregate — топ контрагентов по сумме платежей.
func (s *CompanyStore) aggregate(edges []paymentEdge, role string, top int) ([]Counterparty, float64) {
	sum := map[string]*Counterparty{}
	total := 0.0
	for _, e := range edges {
		cp, ok := sum[e.Other]
		if !ok {
			cp = &Counterparty{ID: e.Other, Role: role}
			sum[e.Other] = cp
		}
		cp.Amount += e.Amount
		cp.Count += e.Count
		total += e.Amount
	}
	list := make([]Counterparty, 0, len(sum))
	for _, cp := range sum {
		if total > 0 {
			cp.Share = round(cp.Amount/total, 4)
		}
		if oc, ok := s.Companies[cp.ID]; ok {
			cp.Sector, cp.SectorNm, cp.Region = oc.Sector, sectorName(oc.Sector), oc.Region
		} else {
			cp.Sector, cp.SectorNm = "", "нет данных"
		}
		list = append(list, *cp)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Amount > list[j].Amount })
	if len(list) > top {
		list = list[:top]
	}
	return list, total
}

// Profile строит бизнес-контекст компании (с кэшем).
func (s *CompanyStore) Profile(id string) (*CompanyProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.cache[id]; ok {
		return p, nil
	}
	c, ok := s.Companies[id]
	if !ok {
		return nil, fmt.Errorf("компания %s не найдена", id)
	}
	p := &CompanyProfile{Company: *c, SectorName: sectorName(c.Sector), CounterSectors: map[string]float64{}, Synthetic: s.Synthetic}
	p.IsLarge = c.RevenueKnown && c.Revenue >= largeBusinessRevenue
	switch {
	case !c.RevenueKnown:
		p.Scale = "масштаб не определён (нет выручки)"
		p.DataQuality = append(p.DataQuality, "нет данных о выручке")
	case p.IsLarge:
		p.Scale = "крупный бизнес (выручка " + formatRub(c.Revenue) + ")"
	default:
		p.Scale = "выручка " + formatRub(c.Revenue) + " — ниже порога крупного бизнеса (15 млрд ₽)"
	}
	if c.OKVEDCode == "" {
		p.DataQuality = append(p.DataQuality, "не указан основной ОКВЭД — отрасль определена как «прочее»")
	}

	// Контрагенты.
	p.Suppliers, _ = s.aggregate(s.outgoing[id], "supplier", 8)
	p.Customers, _ = s.aggregate(s.incoming[id], "customer", 8)
	all, total := s.aggregate(append(append([]paymentEdge{}, s.outgoing[id]...), s.incoming[id]...), "any", 1<<30)
	for _, cp := range all {
		if cp.Sector != "" && total > 0 {
			p.CounterSectors[cp.Sector] += cp.Amount / total
		}
	}
	for k, v := range p.CounterSectors {
		p.CounterSectors[k] = round(v, 3)
	}
	if len(all) == 0 {
		p.DataQuality = append(p.DataQuality, "нет данных о платежах — контрагенты не учитываются")
	}

	// Группа компаний и связи через физлиц.
	if c.GroupID != "" {
		for _, m := range s.groups[c.GroupID] {
			if m != id {
				p.GroupMembers = append(p.GroupMembers, m)
			}
		}
	}
	persons := uniqueStrings(s.personsOf[id])
	p.LinkedPersons = len(persons)
	var linked []string
	for _, f := range persons {
		for _, u := range s.companiesOf[f] {
			if u != id {
				linked = append(linked, u)
			}
		}
	}
	p.LinkedCompanies = uniqueStrings(linked)
	// География связанных физлиц (CITY_NAME — город, RC_NAME — макрорегион банка):
	// показывает, где ещё у бизнеса есть люди и, вероятно, площадки.
	p.PersonCities = map[string]int{}
	macro := map[string]int{}
	for _, f := range persons {
		if g, ok := s.personGeo[f]; ok {
			if city := strings.TrimSpace(g[0]); city != "" && !strings.Contains(strings.ToLower(city), "не опред") {
				p.PersonCities[city]++
			}
			if rc := strings.TrimSpace(g[1]); rc != "" && !strings.Contains(strings.ToLower(rc), "не опред") {
				macro[rc]++
			}
		}
	}
	for k := range macro {
		p.MacroRegions = append(p.MacroRegions, capitalize(strings.ToLower(k)))
	}
	sort.Strings(p.MacroRegions)
	if p.Company.City == "" && len(p.PersonCities) > 0 {
		best, n := "", 0
		for k, v := range p.PersonCities {
			if v > n || v == n && k < best {
				best, n = k, v
			}
		}
		p.Company.City = best
		p.DataQuality = append(p.DataQuality, "город компании не указан — определён по связанным физлицам")
	}
	if len(p.LinkedCompanies) > 20 {
		p.LinkedCompanies = p.LinkedCompanies[:20]
	}

	// Пустые списки отдаём как [] (а не null) — удобнее для API/UI.
	for _, l := range []*[]Counterparty{&p.Suppliers, &p.Customers} {
		if *l == nil {
			*l = []Counterparty{}
		}
	}
	for _, l := range []*[]string{&p.GroupMembers, &p.LinkedCompanies, &p.DataQuality} {
		if *l == nil {
			*l = []string{}
		}
	}
	p.Features = buildFeatures(p)
	p.Exposures = buildExposures(p)
	s.cache[id] = p
	return p, nil
}

func buildFeatures(p *CompanyProfile) []Feature {
	c := p.Company
	f := []Feature{
		{"Отрасль", p.SectorName, "MAINOKVED_CODE"},
		{"Основной ОКВЭД", strings.TrimSpace(c.OKVEDCode + " " + c.OKVEDName), "MAINOKVED_NAME"},
	}
	if sp := specializationFor(c.OKVEDCode); sp != nil {
		f = append(f, Feature{"Специализация", sp.Label, "MAINOKVED_CODE"})
	}
	f = append(f, []Feature{
		{"Масштаб", p.Scale, "REVENUE_COMPANY"},
	}...)
	add := func(name, val, field string) {
		if val != "" {
			f = append(f, Feature{name, val, field})
		}
	}
	add("Тип организации", map[string]string{"КОММ_ОРГ": "коммерческая организация"}[c.OrgType]+map[bool]string{true: c.OrgType}[c.OrgType != "КОММ_ОРГ"], "ORG_TYPE")
	add("Форма собственности", capitalize(strings.ToLower(c.OKFS)), "OKFS_NAME")
	if c.Age != "" {
		add("Возраст компании", c.Age+" лет", "COMPANY_AGE_YEARS_GRADATION")
	}
	add("Стадия развития", c.EcoStage, "ECO_MARK_LABEL")
	if c.WorkersFNS != "" {
		add("Численность (ФНС)", strings.ToLower(c.WorkersFNS)+" сотрудников", "AMOUNT_WORKERS_FNS_GRADATION")
	}
	add("Регион", strings.Trim(c.Region+", "+c.City, ", "), "REGION")
	var other []string
	for r := range p.PersonCities {
		if !strings.Contains(c.City, r) {
			other = append(other, r)
		}
	}
	sort.Strings(other)
	if len(other) > 0 {
		add("Города связанных лиц", strings.Join(other, ", "), "CITY_NAME")
	}
	if len(p.MacroRegions) > 0 {
		add("Макрорегион", strings.Join(p.MacroRegions, ", "), "RC_NAME")
	}
	add("Статус в банке", map[string]string{"CLIENT": "действующий клиент", "PROSPECT": "потенциальный клиент",
		"CLOSE_CLIENT": "бывший клиент"}[c.ClientType], "CL_TYPE")
	if c.NegativeMark != "" && parseFlag(c.NegativeMark) {
		add("Негативные отметки", "есть — учитывайте при оценке контрагентских и кредитных рисков", "NEGATIVEMARK")
	}
	for _, code := range []string{"LOA", "LEA", "DEP", "GAR", "FACTORING", "ZPP", "RKO"} {
		if c.Products[code] {
			add("Продукт банка", productNames[code], productFields[code])
		}
	}
	if c.FTSFlag {
		v := "есть данные о ВЭД в базе ФТС"
		if c.FTSCountry != "" {
			v += " (страна: " + c.FTSCountry + ")"
		}
		add("Внешнеэкономическая деятельность", v, "FTS_FLAG")
	}
	if c.GroupID != "" {
		add("Группа компаний", fmt.Sprintf("%s (%d компаний в группе)", c.GroupID, len(p.GroupMembers)+1), "GROUP_ID")
	}
	keys := sortedKeys(p.CounterSectors)
	sort.Slice(keys, func(i, j int) bool { return p.CounterSectors[keys[i]] > p.CounterSectors[keys[j]] })
	var parts []string
	for i, k := range keys {
		if i >= 3 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s — %.0f%%", sectorName(k), p.CounterSectors[k]*100))
	}
	if len(parts) > 0 {
		add("Отрасли контрагентов (по платежам)", strings.Join(parts, "; "), "df_Uid_payment_hack")
	}
	return f
}

// buildExposures объединяет чувствительности: отрасль + продукты банка + ВЭД + контрагенты.
func buildExposures(p *CompanyProfile) []Exposure {
	c := p.Company
	acc := map[string]*Exposure{}
	add := func(factor string, sign, w float64, reason, field, up, down string) {
		e, ok := acc[factor]
		if !ok {
			e = &Exposure{Factor: factor, FactorName: factorName(factor)}
			acc[factor] = e
		}
		// Знак — взвешенное среднее, вес — «вероятностное ИЛИ» (не превышает 1).
		e.Sign = (e.Sign*e.Weight + sign*w) / math.Max(e.Weight+w, 1e-9)
		e.Weight = 1 - (1-e.Weight)*(1-w)
		e.Reasons = append(e.Reasons, reason)
		e.Fields = uniqueStrings(append(e.Fields, field))
		e.Channels = append(e.Channels, Channel{Reason: reason, Field: field, Sign: sign, Weight: w, EffectUp: up, EffectDown: down})
		if e.EffectUp == "" || w >= 0.7 {
			e.EffectUp, e.EffectDown = up, down
		}
	}

	if s, ok := sectorIndex[c.Sector]; ok {
		for _, sv := range s.Sens {
			add(sv.Factor, sv.Sign, sv.Weight, "отрасль компании — "+strings.ToLower(s.Name), "MAINOKVED_CODE", sv.EffectUp, sv.EffectDn)
		}
	}
	if sp := specializationFor(c.OKVEDCode); sp != nil {
		for _, sv := range sp.Sens {
			add(sv.Factor, sv.Sign, sv.Weight, "специализация — "+sp.Label+" (ОКВЭД "+c.OKVEDCode+")", "MAINOKVED_CODE", sv.EffectUp, sv.EffectDn)
		}
	}
	if c.Products["LOA"] {
		add("key_rate", -1, 0.9, "у компании есть кредитные продукты", "CL_PRODUCT_LOA_MARK",
			"растут расходы на обслуживание долга и стоимость нового финансирования", "снижаются процентные расходы, открывается окно для рефинансирования")
		add("credit_market", -1, 0.7, "у компании есть кредитные продукты", "CL_PRODUCT_LOA_MARK",
			"дорожает рефинансирование", "дешевеет рефинансирование")
	}
	if c.Products["LEA"] {
		add("key_rate", -1, 0.5, "у компании есть лизинг", "CL_PRODUCT_LEA_MARK",
			"дорожают новые лизинговые сделки", "дешевеют новые лизинговые сделки")
	}
	if c.Products["FACTORING"] {
		add("key_rate", -1, 0.4, "компания использует факторинг", "FACTORING_MARK",
			"дорожает финансирование дебиторской задолженности", "дешевеет финансирование дебиторской задолженности")
	}
	if c.Products["DEP"] {
		add("key_rate", +1, 0.5, "у компании есть депозиты", "CL_PRODUCT_DEP_MARK",
			"растёт доходность размещения свободной ликвидности", "снижается доходность депозитов")
	}
	if c.Products["GAR"] {
		add("regulation", 0, 0.3, "компания использует банковские гарантии (вероятно, госзаказ/тендеры)", "CL_PRODUCT_GAR_MARK",
			"меняются требования к обеспечению контрактов", "меняются требования к обеспечению контрактов")
	}
	if c.Products["ZPP"] {
		add("labor", -1, 0.5, "у компании зарплатный проект (значимый ФОТ)", "CL_PRODUCT_ZPP_MARK",
			"растут расходы на оплату труда", "снижается давление на ФОТ")
	}
	if c.FTSFlag {
		sign, w := 0.0, 0.8
		if e, ok := acc["fx_rub"]; ok {
			sign = e.Sign
		}
		add("fx_rub", sign, w, "компания ведёт внешнеэкономическую деятельность", "FTS_FLAG",
			"меняется рублёвая стоимость валютных контрактов", "меняется рублёвая стоимость валютных контрактов")
		add("sanctions_trade", -1, 0.8, "компания ведёт внешнеэкономическую деятельность", "FTS_FLAG",
			"усложняются поставки и расчёты по ВЭД", "упрощаются поставки и расчёты по ВЭД")
		add("logistics", -1, 0.4, "компания ведёт внешнеэкономическую деятельность", "FTS_FLAG",
			"дорожает международная логистика", "дешевеет международная логистика")
	}
	if w := strings.ToLower(c.WorkersFNS + " " + c.WorkersSPARK); strings.Contains(w, "более 251") || strings.Contains(w, "101-250") ||
		strings.Contains(w, "1000") || strings.Contains(w, "500") {
		add("labor", -1, 0.4, "большая численность персонала", "AMOUNT_WORKERS_FNS_GRADATION",
			"растут расходы на оплату труда", "снижается давление на ФОТ")
	}
	// Контрагенты: отрасли, на которые приходится заметная доля оборота. Канал косвенный:
	// фактор меняет положение контрагентов, а через них — спрос и платёжную дисциплину.
	// Берём только сильные чувствительности отрасли контрагента (вес ≥ 0,8) и с пониженным весом.
	for sec, share := range p.CounterSectors {
		if share < 0.15 || sec == c.Sector {
			continue
		}
		s, ok := sectorIndex[sec]
		if !ok {
			continue
		}
		for _, sv := range s.Sens {
			if sv.Weight < 0.8 {
				continue
			}
			add(sv.Factor, 0, clamp(share*sv.Weight*0.7, 0.1, 0.45),
				fmt.Sprintf("%.0f%% платёжного оборота — контрагенты из отрасли «%s», для которой это ключевой фактор", share*100, s.Name),
				"df_Uid_payment_hack",
				fmt.Sprintf("у контрагентов из отрасли «%s» %s — это может изменить их заказы и сроки оплаты", s.Name, sv.EffectUp),
				fmt.Sprintf("у контрагентов из отрасли «%s» %s — это может изменить их заказы и сроки оплаты", s.Name, sv.EffectDn))
		}
	}
	// Базовые макрофакторы — для всех компаний с небольшим весом.
	add("gdp_growth", +1, 0.25, "общая чувствительность бизнеса к экономическому циклу", "—",
		"растёт спрос в экономике", "снижается спрос в экономике")
	add("taxes", -1, 0.25, "все компании — налогоплательщики", "—",
		"растёт налоговая нагрузка", "снижается налоговая нагрузка")
	add("inflation", -1, 0.2, "издержки компании зависят от инфляции", "—",
		"растут издержки", "снижается давление издержек")
	add("regulation", 0, 0.2, "любая компания подвержена изменениям регулирования", "—",
		"растут затраты на соответствие", "снижаются затраты на соответствие")

	out := make([]Exposure, 0, len(acc))
	for _, e := range acc {
		e.Weight = round(e.Weight, 3)
		e.Sign = round(e.Sign, 2)
		e.Reasons = uniqueStrings(e.Reasons)
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		return out[i].Factor < out[j].Factor
	})
	return out
}
