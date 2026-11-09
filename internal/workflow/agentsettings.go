// Настройка агентов проекта (CLAB-425, решение 0077 п. 4): какие модели
// доступны команде у каждого агента, какая модель и усилие стоят на уровне
// low|medium|high, по какому прайсу считаются суммы. Данные проекта версиями
// по образцу цикла (решение 0016): правится черновик, активация одной
// транзакцией снимает прежнюю версию.
//
// Сервер таблицу хранит и отдаёт, но модель не выбирает, не проверяет и не
// принуждает: уровень остаётся рекомендацией (0060, п. 10). Захват пишет на
// прогон версию-снимок (ClaimWith → snapshotExecution) и отдаёт в execution
// модель уровня этапа для каждого агента.
//
// Проверка документа — одна функция ValidateAgentSettings на все входы:
// заведение черновика с документом, замена, активация и импорт связки
// (CLAB-428). Секрет в документе — отказ с путём, без значения в ответе и
// журнале: журнал пишет документ размером и sha256 (auditDigestActions), а
// «было → стало» — из принятых версий.
package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Пределы документа (решение 0077, п. 4.2). Размер — по JSON значения
// document, а не тела запроса: так REST и MCP считают одно и то же (Q3
// аналитики CLAB-425).
const (
	AgentSettingsMaxBytes     = 64 << 10
	agentSettingsMaxChangelog = 2000
	agentSettingsMaxModel     = 200
	agentSettingsMaxEffort    = 32
	agentSettingsMaxPrice     = 1_000_000
)

// KnownAgents — имена агентов, которые знает набор инстанса. До привязок
// набора (CLAB-417) — константа (Q2 аналитики CLAB-425); неизвестное имя —
// предупреждение, а не отказ: компания ведёт свой агент на привязке generic.
var KnownAgents = []string{"claude", "codex", "generic"}

// agentSettingsSecretWords — части имени поля или ключа, по которым он
// считается секретом. Перечень — константа сервера: расширение — правка кода
// с тестом, без нового решения (0077, п. 4.2).
var agentSettingsSecretWords = []string{"token", "secret", "api_key", "apikey", "password"}

// agentSettingsTokenPrefix — префикс токена Cadence Lab: строка с ним —
// секрет под любым ключом.
const agentSettingsTokenPrefix = "sdlc_"

var (
	agentNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	effortPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	asOfPattern      = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

// Поля документа по уровням: неизвестное поле — отказ, опечатка не должна
// молча выключать настройку.
var (
	agentSettingsRootFields  = []string{"agents", "pricing"}
	agentSettingsAgentFields = []string{"models", "levels"}
	agentSettingsLevelFields = []string{"model", "effort"}
	agentSettingsPriceFields = []string{"as_of", "models"}
	agentSettingsModelPrice  = []string{"input", "output", "cache_read", "cache_write"}
)

// AgentModel — модель агента на уровне этапа в ответе захвата.
type AgentModel struct {
	Model  string `json:"model"`
	Effort string `json:"effort,omitempty"`
}

// agentSettingsDoc — проверенный документ для чтения: ответ захвата берёт
// из него модели уровня.
type agentSettingsDoc struct {
	Agents map[string]struct {
		Levels map[string]AgentModel `json:"levels"`
	} `json:"agents"`
}

// agentSettingsInvalid — отказ проверки: путь поля в details.path и причина.
// Значения в тексте нет никогда: отвергнутое как секрет не должно попасть ни
// в ответ, ни в журнал, а остальным значение и так известно отправителю.
func agentSettingsInvalid(path, reason string) *apperr.Error {
	where := path
	if where == "" {
		where = "документ"
	}
	return apperr.InvalidInput("настройка агентов, %s: %s", where, reason).
		WithDetails(map[string]any{"path": path})
}

func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

func indexPath(parent string, i int) string {
	return parent + "[" + strconv.Itoa(i) + "]"
}

// secretKeyPath — путь к ключу, отвергнутому как секрет. Ключ с префиксом
// токена — сам секрет, поэтому в путь он не идёт, только его место.
func secretKeyPath(parent, key string) string {
	if strings.HasPrefix(strings.ToLower(key), agentSettingsTokenPrefix) {
		return joinPath(parent, "["+agentSettingsTokenPrefix+"…]")
	}
	return joinPath(parent, key)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// decodeAgentSettings разбирает документ: один JSON-объект, числа как есть.
func decodeAgentSettings(raw json.RawMessage) (map[string]any, error) {
	if len(raw) > AgentSettingsMaxBytes {
		return nil, agentSettingsInvalid("", fmt.Sprintf("документ больше %d КиБ (%d байт JSON)",
			AgentSettingsMaxBytes>>10, len(raw)))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, agentSettingsInvalid("", "документ — JSON-объект, а это не разбирается как JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, agentSettingsInvalid("", "документ — один JSON-объект, после него лишнее")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, agentSettingsInvalid("", "документ — JSON-объект {\"agents\": …, \"pricing\": …}")
	}
	return obj, nil
}

// ValidateAgentSettings проверяет документ настройки агентов по решению
// 0077, п. 4.2, и отдаёт предупреждения (агент, неизвестный набору). Отказ —
// invalid_input с details.path. Её зовут все входы записи; импорт связки
// (CLAB-428) зовёт её же.
func ValidateAgentSettings(raw json.RawMessage) ([]string, error) {
	doc, err := decodeAgentSettings(raw)
	if err != nil {
		return nil, err
	}
	// Секреты — раньше прочих проверок: иначе секрет в неизвестном поле
	// вернулся бы отказом «неизвестное поле» с ключом-секретом в пути.
	if err := findAgentSettingsSecret(doc, ""); err != nil {
		return nil, err
	}
	if err := knownFields(doc, "", agentSettingsRootFields); err != nil {
		return nil, err
	}
	warnings := []string{}
	if raw, ok := doc["agents"]; ok {
		agents, ok := raw.(map[string]any)
		if !ok {
			return nil, agentSettingsInvalid("agents", "объект «имя агента → агент»")
		}
		for _, name := range sortedKeys(agents) {
			if err := checkAgent(name, agents[name]); err != nil {
				return nil, err
			}
			if !knownAgent(name) {
				warnings = append(warnings, fmt.Sprintf("агент %s не известен набору инстанса (известны %s)",
					name, strings.Join(KnownAgents, ", ")))
			}
		}
	}
	if raw, ok := doc["pricing"]; ok {
		if err := checkPricing(raw); err != nil {
			return nil, err
		}
	}
	return warnings, nil
}

func knownAgent(name string) bool {
	for _, a := range KnownAgents {
		if a == name {
			return true
		}
	}
	return false
}

// findAgentSettingsSecret ищет секрет на любой глубине: ключ с секретным
// словом или префиксом токена и строковое значение с префиксом токена.
func findAgentSettingsSecret(v any, path string) error {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(x) {
			lk := strings.ToLower(k)
			if strings.HasPrefix(lk, agentSettingsTokenPrefix) {
				return agentSettingsInvalid(secretKeyPath(path, k),
					"ключ похож на токен Cadence Lab (префикс sdlc_): секреты в настройке агентов не хранятся")
			}
			for _, w := range agentSettingsSecretWords {
				if strings.Contains(lk, w) {
					return agentSettingsInvalid(joinPath(path, k),
						"имя похоже на секрет (содержит "+w+"): секреты в настройке агентов не хранятся")
				}
			}
			if err := findAgentSettingsSecret(x[k], joinPath(path, k)); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range x {
			if err := findAgentSettingsSecret(item, indexPath(path, i)); err != nil {
				return err
			}
		}
	case string:
		if strings.HasPrefix(strings.ToLower(x), agentSettingsTokenPrefix) {
			return agentSettingsInvalid(path,
				"значение похоже на токен Cadence Lab (префикс sdlc_): секреты в настройке агентов не хранятся")
		}
	}
	return nil
}

func knownFields(obj map[string]any, path string, allowed []string) error {
	for _, k := range sortedKeys(obj) {
		ok := false
		for _, a := range allowed {
			if a == k {
				ok = true
				break
			}
		}
		if !ok {
			return agentSettingsInvalid(joinPath(path, k),
				"поле не поддерживается, допустимы "+strings.Join(allowed, ", "))
		}
	}
	return nil
}

func checkAgent(name string, raw any) error {
	path := joinPath("agents", name)
	if !agentNamePattern.MatchString(name) {
		return agentSettingsInvalid(path, "имя агента — строчная латиница, цифры, _ и -, с буквы, до 32 символов")
	}
	agent, ok := raw.(map[string]any)
	if !ok {
		return agentSettingsInvalid(path, "объект {\"models\": […], \"levels\": {…}}")
	}
	if err := knownFields(agent, path, agentSettingsAgentFields); err != nil {
		return err
	}
	var models map[string]bool
	if rawModels, ok := agent["models"]; ok {
		list, ok := rawModels.([]any)
		if !ok {
			return agentSettingsInvalid(joinPath(path, "models"), "массив полных id моделей")
		}
		models = map[string]bool{}
		for i, item := range list {
			p := indexPath(joinPath(path, "models"), i)
			if err := checkModelID(item, p); err != nil {
				return err
			}
			models[item.(string)] = true
		}
	}
	rawLevels, ok := agent["levels"]
	if !ok {
		return nil
	}
	levelsPath := joinPath(path, "levels")
	levels, ok := rawLevels.(map[string]any)
	if !ok {
		return agentSettingsInvalid(levelsPath, "объект «уровень → {model, effort}»")
	}
	for _, level := range sortedKeys(levels) {
		lp := joinPath(levelsPath, level)
		if !domain.ValidModelLevel(level) {
			return agentSettingsInvalid(lp, "уровень не поддерживается, допустимы "+strings.Join(domain.ModelLevels, ", "))
		}
		entry, ok := levels[level].(map[string]any)
		if !ok {
			return agentSettingsInvalid(lp, "объект {\"model\": …, \"effort\": …}")
		}
		if err := knownFields(entry, lp, agentSettingsLevelFields); err != nil {
			return err
		}
		model, ok := entry["model"]
		if !ok {
			return agentSettingsInvalid(joinPath(lp, "model"), "модель уровня обязательна")
		}
		if err := checkModelID(model, joinPath(lp, "model")); err != nil {
			return err
		}
		if models != nil && !models[model.(string)] {
			return agentSettingsInvalid(joinPath(lp, "model"), "модели нет в models этого агента")
		}
		if effort, ok := entry["effort"]; ok {
			if err := checkEffort(effort, joinPath(lp, "effort")); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkModelID — полный id модели: непустая строка до 200 символов без
// пробелов и управляющих символов. Сервер имена не нормализует.
func checkModelID(v any, path string) error {
	s, ok := v.(string)
	if !ok {
		return agentSettingsInvalid(path, "модель — строка, полный id модели")
	}
	return checkModelString(s, path)
}

func checkModelString(s, path string) error {
	if s == "" {
		return agentSettingsInvalid(path, "модель пустая")
	}
	if utf8.RuneCountInString(s) > agentSettingsMaxModel {
		return agentSettingsInvalid(path, fmt.Sprintf("модель длиннее %d символов", agentSettingsMaxModel))
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == utf8.RuneError {
			return agentSettingsInvalid(path, "в id модели пробел или управляющий символ")
		}
	}
	return nil
}

func checkEffort(v any, path string) error {
	s, ok := v.(string)
	switch {
	case !ok:
		return agentSettingsInvalid(path, "effort — строка")
	case s == "":
		return agentSettingsInvalid(path, "effort пустой")
	case len(s) > agentSettingsMaxEffort:
		return agentSettingsInvalid(path, fmt.Sprintf("effort длиннее %d символов", agentSettingsMaxEffort))
	case !effortPattern.MatchString(s):
		return agentSettingsInvalid(path, "effort — латиница, цифры, _ и -")
	}
	return nil
}

func checkPricing(raw any) error {
	pricing, ok := raw.(map[string]any)
	if !ok {
		return agentSettingsInvalid("pricing", "объект {\"as_of\": \"YYYY-MM-DD\", \"models\": {…}}")
	}
	if err := knownFields(pricing, "pricing", agentSettingsPriceFields); err != nil {
		return err
	}
	asOf, ok := pricing["as_of"].(string)
	if !ok || !asOfPattern.MatchString(asOf) {
		return agentSettingsInvalid("pricing.as_of", "дата YYYY-MM-DD обязательна при pricing")
	}
	if _, err := time.Parse(time.DateOnly, asOf); err != nil {
		return agentSettingsInvalid("pricing.as_of", "дата YYYY-MM-DD обязательна при pricing")
	}
	rawModels, ok := pricing["models"]
	if !ok {
		return nil
	}
	models, ok := rawModels.(map[string]any)
	if !ok {
		return agentSettingsInvalid("pricing.models", "объект «полный id модели → цены»")
	}
	for _, id := range sortedKeys(models) {
		mp := joinPath("pricing.models", id)
		if err := checkModelString(id, mp); err != nil {
			return err
		}
		prices, ok := models[id].(map[string]any)
		if !ok {
			return agentSettingsInvalid(mp, "объект {\"input\", \"output\", \"cache_read\"?, \"cache_write\"?}")
		}
		if err := knownFields(prices, mp, agentSettingsModelPrice); err != nil {
			return err
		}
		for _, field := range agentSettingsModelPrice {
			v, ok := prices[field]
			if !ok {
				if field == "input" || field == "output" {
					return agentSettingsInvalid(joinPath(mp, field), "цена обязательна (USD за 1 000 000 токенов)")
				}
				continue
			}
			if err := checkPrice(v, joinPath(mp, field)); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkPrice(v any, path string) error {
	n, ok := v.(json.Number)
	if !ok {
		return agentSettingsInvalid(path, "цена — число, USD за 1 000 000 токенов")
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	switch {
	case err != nil || math.IsInf(f, 0) || math.IsNaN(f):
		return agentSettingsInvalid(path, "цена — конечное число")
	case f < 0:
		return agentSettingsInvalid(path, "цена отрицательная")
	case f > agentSettingsMaxPrice:
		return agentSettingsInvalid(path, "цена больше 1 000 000")
	}
	return nil
}

// CanonicalAgentSettings — канонический JSON документа: ключи по порядку,
// без пробелов, числа в кратчайшей десятичной записи. По нему считается
// sha256 и сравнение «то же самое» у импорта связки (CLAB-428): порядок
// ключей, пробелы и запись числа (1.0, 1, 1e0; jsonb Postgres пишет 1e2 как
// 100, а MCP-вход отдаёт 1.0 как 1) документ не меняют.
func CanonicalAgentSettings(raw json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, apperr.Internal(err, "документ настройки агентов не разбирается")
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(canonicalNumbers(v)); err != nil {
		return nil, apperr.Internal(err, "документ настройки агентов не кодируется")
	}
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

// canonicalNumbers приводит каждое число документа к одной записи. В
// проверенном документе чисел только цены — конечные и не больше
// 1 000 000, float64 их не искажает; что не разобралось (документ, записанный
// в обход сервиса), остаётся как записано.
func canonicalNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		return canonicalNumber(x)
	case map[string]any:
		for k, val := range x {
			x[k] = canonicalNumbers(val)
		}
	case []any:
		for i, val := range x {
			x[i] = canonicalNumbers(val)
		}
	}
	return v
}

func canonicalNumber(n json.Number) json.Number {
	if i, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
		return json.Number(strconv.FormatInt(i, 10))
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return n
	}
	if f == 0 {
		return json.Number("0")
	}
	return json.Number(strconv.FormatFloat(f, 'f', -1, 64))
}

// AgentSettingsSHA256 — sha256 канонического JSON документа.
func AgentSettingsSHA256(raw json.RawMessage) (string, int, error) {
	canon, err := CanonicalAgentSettings(raw)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), len(canon), nil
}

// agentModelsFor — модели уровня level из документа версии: агенты, у
// которых в версии есть запись для этого уровня.
func agentModelsFor(doc json.RawMessage, level string) (map[string]AgentModel, error) {
	var d agentSettingsDoc
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, apperr.Internal(err, "документ настройки агентов не читается")
	}
	out := map[string]AgentModel{}
	for name, agent := range d.Agents {
		if m, ok := agent.Levels[level]; ok && m.Model != "" {
			out[name] = m
		}
	}
	return out, nil
}

// ── виды ────────────────────────────────────────────────────────────────────

// AgentSettingsView — версия целиком (решение 0077, п. 4.6).
type AgentSettingsView struct {
	Project     string          `json:"project"`
	Version     int             `json:"version"`
	State       string          `json:"state"`
	Document    json.RawMessage `json:"document"`
	Changelog   *string         `json:"changelog"`
	SHA256      string          `json:"sha256"`
	CreatedBy   *string         `json:"created_by"`
	CreatedAt   time.Time       `json:"created_at"`
	ActivatedBy *string         `json:"activated_by"`
	ActivatedAt *time.Time      `json:"activated_at"`
	Warnings    []string        `json:"warnings"`
}

// AgentSettingsListItem — строка списка версий: без документа, с sha256.
type AgentSettingsListItem struct {
	Version     int        `json:"version"`
	State       string     `json:"state"`
	Changelog   *string    `json:"changelog"`
	SHA256      string     `json:"sha256"`
	CreatedBy   *string    `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	ActivatedBy *string    `json:"activated_by"`
	ActivatedAt *time.Time `json:"activated_at"`
}

// AgentSettingsActivation — ответ активации: номер активированной и снятой
// версии (null — активной не было).
type AgentSettingsActivation struct {
	Activated AgentSettingsView `json:"activated"`
	Retired   *int              `json:"retired"`
}

// AgentSettingsTrace — версия в журнале действий: номер, состояние, размер и
// sha256 канонического документа вместо документа целиком (Q1 аналитики
// CLAB-425: документ до 64 КиБ дважды в записи не уложился бы в предел
// журнала).
type AgentSettingsTrace struct {
	Version   int     `json:"version"`
	State     string  `json:"state"`
	SHA256    string  `json:"sha256"`
	Size      int     `json:"size"`
	Changelog *string `json:"changelog,omitempty"`
}

// AgentSettingsChange — «было → стало» для журнала, одно для REST и MCP.
type AgentSettingsChange struct {
	Action  string              `json:"action"`
	Version int                 `json:"version"`
	Before  *AgentSettingsTrace `json:"before"`
	After   *AgentSettingsTrace `json:"after"`
	Retired *int                `json:"retired,omitempty"`
}

// Действия журнала с настройкой агентов.
const (
	AgentSettingsCreated   = "draft_created"
	AgentSettingsReplaced  = "draft_replaced"
	AgentSettingsDiscarded = "draft_discarded"
	AgentSettingsActivated = "activated"
)

func agentSettingsView(project string, v domain.AgentSettingsVersion, warnings []string) (AgentSettingsView, error) {
	sum, _, err := AgentSettingsSHA256(v.Document)
	if err != nil {
		return AgentSettingsView{}, err
	}
	if warnings == nil {
		warnings = []string{}
	}
	return AgentSettingsView{
		Project: project, Version: v.Version, State: v.State, Document: v.Document,
		Changelog: v.Changelog, SHA256: sum, CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt,
		ActivatedBy: v.ActivatedBy, ActivatedAt: v.ActivatedAt, Warnings: warnings,
	}, nil
}

// storedView — версия из базы с предупреждениями по её документу:
// предупреждение об агенте видно и при чтении, а не только при записи.
func storedView(project string, v domain.AgentSettingsVersion) (AgentSettingsView, error) {
	warnings, err := ValidateAgentSettings(v.Document)
	if err != nil {
		// Документ, записанный в обход сервиса, читается как есть: его
		// отобьёт активация, а читающему нужно увидеть, что там лежит.
		warnings = []string{"документ не проходит проверку: " + apperr.As(err).Message}
	}
	return agentSettingsView(project, v, warnings)
}

func agentSettingsTrace(v domain.AgentSettingsVersion) (*AgentSettingsTrace, error) {
	sum, size, err := AgentSettingsSHA256(v.Document)
	if err != nil {
		return nil, err
	}
	return &AgentSettingsTrace{Version: v.Version, State: v.State, SHA256: sum, Size: size, Changelog: v.Changelog}, nil
}

// ── права ───────────────────────────────────────────────────────────────────

// AgentSettingsWriteRight — вправе ли актор править настройку агентов
// (решение 0077, п. 4.9). Правит человек уровня maintainer и выше; бот —
// нет даже с уровнем admin: прайс влияет на бюджет и суммы, агент не должен
// удешевлять себя сам (как у политики учёта). Правило вида принципала — в
// любом режиме; в режиме заголовка уровень максимальный, и решает вид.
func AgentSettingsWriteRight(a Actor) error {
	switch a.Class() {
	case ClassTaskToken:
		return executorRefused(a.Bound, "правка настройки агентов проекта")
	case ClassUnchecked:
		return nil
	case ClassMaintainer:
		if a.Principal.Kind == domain.PrincipalUser {
			return nil
		}
		return agentSettingsHumanOnly(a)
	case ClassBot:
		return agentSettingsHumanOnly(a)
	}
	have := string(a.Level)
	if have == "" {
		have = "нет участия"
	}
	return apperr.Forbidden("настройку агентов проекта правит сопровождающий (уровень maintainer или admin), а есть %s", have).
		WithDetails(map[string]any{"reason": DenyLevel})
}

func agentSettingsHumanOnly(a Actor) error {
	kind := string(a.Principal.Kind)
	if kind == "" {
		kind = "без вида"
	}
	return apperr.Forbidden("настройку агентов проекта правит человек — сопровождающий проекта; "+
		"принципал вида %s её не правит даже с уровнем %s", kind, a.Level).
		WithDetails(map[string]any{"reason": DenyHumanAction})
}

// ── чтение ──────────────────────────────────────────────────────────────────

// AgentSettings — активная версия проекта (version nil) или версия по
// номеру, в том числе черновик. Активной нет — nil без ошибки: «настройки
// нет» — нормальное состояние.
func (e *Engine) AgentSettings(ctx context.Context, projectKey string, version *int) (*AgentSettingsView, error) {
	q := e.store.DB()
	project, err := e.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return nil, err
	}
	var v domain.AgentSettingsVersion
	if version != nil {
		if v, err = e.store.AgentSettingsByVersion(ctx, q, project.ID, *version); err != nil {
			return nil, err
		}
	} else if v, err = e.store.ActiveAgentSettings(ctx, q, project.ID); err != nil {
		if apperr.As(err).Code == apperr.CodeNotFound {
			return nil, nil
		}
		return nil, err
	}
	view, err := storedView(project.Key, v)
	if err != nil {
		return nil, err
	}
	return &view, nil
}

// AgentSettingsVersions — все версии проекта без документов.
func (e *Engine) AgentSettingsVersions(ctx context.Context, projectKey string) ([]AgentSettingsListItem, error) {
	q := e.store.DB()
	project, err := e.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return nil, err
	}
	versions, err := e.store.ListAgentSettings(ctx, q, project.ID)
	if err != nil {
		return nil, err
	}
	out := make([]AgentSettingsListItem, 0, len(versions))
	for _, v := range versions {
		sum, _, err := AgentSettingsSHA256(v.Document)
		if err != nil {
			return nil, err
		}
		out = append(out, AgentSettingsListItem{
			Version: v.Version, State: v.State, Changelog: v.Changelog, SHA256: sum,
			CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt, ActivatedBy: v.ActivatedBy, ActivatedAt: v.ActivatedAt,
		})
	}
	return out, nil
}

// ── запись ──────────────────────────────────────────────────────────────────

// Откуда черновик без документа.
const (
	AgentSettingsFromActive = "active"
	AgentSettingsFromEmpty  = "empty"
)

// AgentSettingsDraftInput — заведение черновика: document важнее from; без
// обоих — копия активной версии или пустой документ.
type AgentSettingsDraftInput struct {
	From      string
	Document  json.RawMessage
	Changelog *string
}

// AgentSettingsPutInput — замена документа черновика целиком; changelog nil
// — прежний.
type AgentSettingsPutInput struct {
	Document  json.RawMessage
	Changelog *string
}

func checkChangelog(c *string) error {
	if c != nil && utf8.RuneCountInString(*c) > agentSettingsMaxChangelog {
		return agentSettingsInvalid("changelog", fmt.Sprintf("длиннее %d символов", agentSettingsMaxChangelog))
	}
	return nil
}

// agentSettingsWrite — общая часть записи: право актора, проект, транзакция.
func (e *Engine) agentSettingsWrite(ctx context.Context, projectKey string, who Actor, fn func(tx pgx.Tx, project domain.Project) error) error {
	if err := AgentSettingsWriteRight(who); err != nil {
		return err
	}
	return e.store.InTx(ctx, func(tx pgx.Tx) error {
		project, err := e.store.GetProjectByKey(ctx, tx, projectKey)
		if err != nil {
			return err
		}
		return fn(tx, project)
	})
}

// CreateAgentSettingsDraft заводит черновик (UC-1). Черновик уже есть —
// conflict, его держит индекс схемы.
func (e *Engine) CreateAgentSettingsDraft(ctx context.Context, projectKey string, who Actor, in AgentSettingsDraftInput) (AgentSettingsView, AgentSettingsChange, error) {
	var view AgentSettingsView
	var change AgentSettingsChange
	err := e.agentSettingsWrite(ctx, projectKey, who, func(tx pgx.Tx, project domain.Project) error {
		var err error
		view, change, err = e.createDraft(ctx, tx, project, who, in)
		return err
	})
	return view, change, err
}

func (e *Engine) createDraft(ctx context.Context, tx pgx.Tx, project domain.Project, who Actor, in AgentSettingsDraftInput) (AgentSettingsView, AgentSettingsChange, error) {
	var view AgentSettingsView
	var change AgentSettingsChange
	if in.From != "" && in.From != AgentSettingsFromActive && in.From != AgentSettingsFromEmpty {
		return view, change, agentSettingsInvalid("from", "допустимо active или empty")
	}
	if err := checkChangelog(in.Changelog); err != nil {
		return view, change, err
	}
	doc := json.RawMessage(`{}`)
	var warnings []string
	switch {
	case len(in.Document) > 0:
		w, err := ValidateAgentSettings(in.Document)
		if err != nil {
			return view, change, err
		}
		doc, warnings = in.Document, w
	case in.From != AgentSettingsFromEmpty:
		active, err := e.store.ActiveAgentSettings(ctx, tx, project.ID)
		switch {
		case err == nil:
			doc = active.Document
			warnings, _ = ValidateAgentSettings(doc)
		case apperr.As(err).Code != apperr.CodeNotFound:
			return view, change, err
		}
	}
	v, err := e.store.InsertAgentSettingsDraft(ctx, tx, project.ID, doc, in.Changelog, who.Principal.ID)
	if err != nil {
		return view, change, err
	}
	if view, err = agentSettingsView(project.Key, v, warnings); err != nil {
		return view, change, err
	}
	after, err := agentSettingsTrace(v)
	if err != nil {
		return view, change, err
	}
	change = AgentSettingsChange{Action: AgentSettingsCreated, Version: v.Version, After: after}
	return view, change, nil
}

// PutAgentSettingsDraft заменяет документ черновика целиком (UC-2).
func (e *Engine) PutAgentSettingsDraft(ctx context.Context, projectKey string, who Actor, in AgentSettingsPutInput) (AgentSettingsView, AgentSettingsChange, error) {
	var view AgentSettingsView
	var change AgentSettingsChange
	err := e.agentSettingsWrite(ctx, projectKey, who, func(tx pgx.Tx, project domain.Project) error {
		draft, err := e.store.LockAgentSettingsDraft(ctx, tx, project.ID)
		if err != nil {
			return err
		}
		view, change, err = e.replaceDraft(ctx, tx, project, draft, in)
		return err
	})
	return view, change, err
}

func (e *Engine) replaceDraft(ctx context.Context, tx pgx.Tx, project domain.Project, draft domain.AgentSettingsVersion, in AgentSettingsPutInput) (AgentSettingsView, AgentSettingsChange, error) {
	var view AgentSettingsView
	var change AgentSettingsChange
	if len(in.Document) == 0 {
		return view, change, agentSettingsInvalid("document", "документ обязателен: черновик заменяется целиком")
	}
	if err := checkChangelog(in.Changelog); err != nil {
		return view, change, err
	}
	warnings, err := ValidateAgentSettings(in.Document)
	if err != nil {
		return view, change, err
	}
	changelog := draft.Changelog
	if in.Changelog != nil {
		changelog = in.Changelog
	}
	v, err := e.ReplaceAgentSettingsVersion(ctx, tx, draft.ID, in.Document, changelog)
	if err != nil {
		return view, change, err
	}
	if view, err = agentSettingsView(project.Key, v, warnings); err != nil {
		return view, change, err
	}
	before, err := agentSettingsTrace(draft)
	if err != nil {
		return view, change, err
	}
	after, err := agentSettingsTrace(v)
	if err != nil {
		return view, change, err
	}
	change = AgentSettingsChange{Action: AgentSettingsReplaced, Version: v.Version, Before: before, After: after}
	return view, change, nil
}

// ReplaceAgentSettingsVersion — запись документа версии по id: единственный
// путь записи документа. Версия не черновик — conflict (правится только
// черновик, решение 0016), документ не меняется.
func (e *Engine) ReplaceAgentSettingsVersion(ctx context.Context, tx pgx.Tx, id uuid.UUID, doc json.RawMessage, changelog *string) (domain.AgentSettingsVersion, error) {
	return e.store.UpdateAgentSettingsDraft(ctx, tx, id, doc, changelog)
}

// UpsertAgentSettingsDraft — запись MCP одним тулом (решение 0077, п. 4.7):
// черновика нет — заводит, есть — заменяет документ и changelog.
func (e *Engine) UpsertAgentSettingsDraft(ctx context.Context, projectKey string, who Actor, in AgentSettingsDraftInput) (AgentSettingsView, AgentSettingsChange, bool, error) {
	var view AgentSettingsView
	var change AgentSettingsChange
	created := false
	err := e.agentSettingsWrite(ctx, projectKey, who, func(tx pgx.Tx, project domain.Project) error {
		draft, err := e.store.LockAgentSettingsDraft(ctx, tx, project.ID)
		switch {
		case err == nil:
			if in.From != "" {
				return agentSettingsInvalid("from", "from — только для заведения черновика, а он уже есть: пришлите document")
			}
			view, change, err = e.replaceDraft(ctx, tx, project, draft, AgentSettingsPutInput{Document: in.Document, Changelog: in.Changelog})
			return err
		case apperr.As(err).Code == apperr.CodeNotFound:
			created = true
			view, change, err = e.createDraft(ctx, tx, project, who, in)
			return err
		}
		return err
	})
	return view, change, created, err
}

// DiscardAgentSettingsDraft удаляет черновик (UC-4); активная и снятые
// версии не трогаются.
func (e *Engine) DiscardAgentSettingsDraft(ctx context.Context, projectKey string, who Actor) (AgentSettingsChange, error) {
	var change AgentSettingsChange
	err := e.agentSettingsWrite(ctx, projectKey, who, func(tx pgx.Tx, project domain.Project) error {
		v, err := e.store.DeleteAgentSettingsDraft(ctx, tx, project.ID)
		if err != nil {
			return err
		}
		before, err := agentSettingsTrace(v)
		if err != nil {
			return err
		}
		change = AgentSettingsChange{Action: AgentSettingsDiscarded, Version: v.Version, Before: before}
		return nil
	})
	return change, err
}

// ActivateAgentSettings активирует черновик (UC-3) одной транзакцией:
// проверка номера и документа, снятие прежней активной, черновик — active.
// version — номер черновика, который видел человек; nil — тот, что есть.
func (e *Engine) ActivateAgentSettings(ctx context.Context, projectKey string, who Actor, version *int) (AgentSettingsActivation, AgentSettingsChange, error) {
	var out AgentSettingsActivation
	var change AgentSettingsChange
	err := e.agentSettingsWrite(ctx, projectKey, who, func(tx pgx.Tx, project domain.Project) error {
		draft, err := e.store.LockAgentSettingsDraft(ctx, tx, project.ID)
		if err != nil {
			return err
		}
		if version != nil && *version != draft.Version {
			return apperr.Conflict("черновик настройки агентов — версия %d, а активировать просили %d: перечитайте черновик",
				draft.Version, *version).WithDetails(map[string]any{"draft_version": draft.Version, "requested": *version})
		}
		// Повторная проверка: документ мог попасть в базу в обход сервиса,
		// а правила проверки — измениться после записи черновика.
		warnings, err := ValidateAgentSettings(draft.Document)
		if err != nil {
			return err
		}
		var before *AgentSettingsTrace
		if active, err := e.store.ActiveAgentSettings(ctx, tx, project.ID); err == nil {
			if before, err = agentSettingsTrace(active); err != nil {
				return err
			}
		} else if apperr.As(err).Code != apperr.CodeNotFound {
			return err
		}
		retired, err := e.store.RetireAgentSettings(ctx, tx, project.ID)
		if err != nil {
			return err
		}
		if err := e.afterRetireHook(); err != nil {
			return err
		}
		v, err := e.store.ActivateAgentSettings(ctx, tx, draft.ID, who.Principal.ID)
		if err != nil {
			return err
		}
		view, err := agentSettingsView(project.Key, v, warnings)
		if err != nil {
			return err
		}
		after, err := agentSettingsTrace(v)
		if err != nil {
			return err
		}
		if before != nil {
			before.State = domain.AgentSettingsActive
		}
		out = AgentSettingsActivation{Activated: view, Retired: retired}
		change = AgentSettingsChange{Action: AgentSettingsActivated, Version: v.Version, Before: before, After: after, Retired: retired}
		return nil
	})
	return out, change, err
}

// afterRetireHook — точка внедрения сбоя для теста отката активации (AC-7):
// сбой после снятия прежней версии обязан откатить и снятие.
func (e *Engine) afterRetireHook() error {
	if e.agentSettingsFault != nil {
		return e.agentSettingsFault()
	}
	return nil
}

// ── снимок и ответ захвата ──────────────────────────────────────────────────

// runAgentSettings — версия настройки для execution прогона: снимок, если
// снимок прогона был (пара уровня записана), иначе текущая активная — то,
// что получит следующий захват. nil — настройки нет.
func (e *Engine) runAgentSettings(ctx context.Context, q postgres.Querier, task domain.Task, run domain.StageRun) (*domain.AgentSettingsVersion, error) {
	if run.ModelLevelSource != nil {
		if run.AgentSettingsVersionID == nil {
			return nil, nil
		}
		v, err := e.store.AgentSettingsByID(ctx, q, *run.AgentSettingsVersionID)
		if err != nil {
			return nil, err
		}
		return &v, nil
	}
	v, err := e.store.ActiveAgentSettings(ctx, q, task.ProjectID)
	if err != nil {
		if apperr.As(err).Code == apperr.CodeNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &v, nil
}

// withAgentSettings дополняет execution версией и моделями уровня: номер
// версии есть всегда (null — настройки не было), models — только при уровне
// и версии.
func withAgentSettings(exec Execution, v *domain.AgentSettingsVersion) (Execution, error) {
	if v == nil {
		return exec, nil
	}
	n := v.Version
	exec.AgentSettingsVersion = &n
	if exec.ModelLevel == nil || *exec.ModelLevel == "" {
		return exec, nil
	}
	models, err := agentModelsFor(v.Document, *exec.ModelLevel)
	if err != nil {
		return exec, err
	}
	exec.Models = &models
	return exec, nil
}
