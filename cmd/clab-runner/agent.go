package main

// Привязка агента раннера (CLAB-420, решение 0077 пп. 3, 8): раннер
// работает тем же механизмом, что чат. Команду, форму стартовой
// инструкции, разбор вывода, лимит подписки, окружение и перевод
// tool_policy даёт привязка набора, а не код раннера; агента раннер
// выбирает по имени программы своей команды или по PATH.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/agentkit"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/agentkit/launch"
)

// Откуда выбрана привязка и откуда набор — значения журнала старта.
const (
	agentFromCommand = "SDLC_RUNNER_COMMAND"
	agentFromPath    = "PATH"
	// Раннер, собранный в обход main: Claude Code, как до привязок.
	agentFromDefault = "по умолчанию"

	bindingsInstance = "инстанс"
	bindingsBuiltin  = "встроенные"
)

// Значения полей привязки, которые читает раннер (agentkit, CLAB-420).
const (
	promptSlash      = "mcp_slash"
	promptText       = "text"
	outputClaudeJSON = launch.OutputClaudeJSON
	outputCodexJSONL = launch.OutputCodexJSONL
)

// agentKitTimeout — предел запроса набора на старте: открытая ручка без
// токена, и ждать её дольше, чем обходиться встроенным набором, незачем.
var agentKitTimeout = 5 * time.Second

// agentKitMaxBytes — предел ответа набора (сейчас около 450 КБ).
const agentKitMaxBytes = 8 << 20

// builtinBindings — привязки набора, встроенного в раннер: тот же пакет
// agentkit, что отдаёт сервер, поэтому копии привязок руками нет.
func builtinBindings() ([]agentkit.Binding, error) {
	k, err := agentkit.Load(version)
	if err != nil {
		return nil, fmt.Errorf("встроенный набор агента: %w", err)
	}
	return k.Bindings, nil
}

// instanceBindings читает привязки GET /v1/agent-kit инстанса без токена.
// Поля, которых нет у сервера прежней версии (prompt и env, CLAB-420),
// берутся у встроенной привязки того же имени: иначе claude инстанса
// v0.28 потерял бы слэш-промпт и ANTHROPIC_* в окружении.
func instanceBindings(ctx context.Context, apiURL string, builtin []agentkit.Binding) ([]agentkit.Binding, error) {
	ctx, cancel := context.WithTimeout(ctx, agentKitTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/agent-kit", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /agent-kit: %s", resp.Status)
	}
	var kit struct {
		Bindings []json.RawMessage `json:"bindings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, agentKitMaxBytes)).Decode(&kit); err != nil {
		return nil, fmt.Errorf("GET /agent-kit: ответ не разбирается: %w", err)
	}
	var out []agentkit.Binding
	for _, raw := range kit.Bindings {
		var b agentkit.Binding
		var keys map[string]json.RawMessage
		if json.Unmarshal(raw, &b) != nil || json.Unmarshal(raw, &keys) != nil || b.Name == "" {
			continue
		}
		own := findBinding(builtin, b.Name)
		if _, ok := keys["prompt"]; !ok && own != nil {
			b.Prompt = own.Prompt
		}
		if _, ok := keys["env"]; !ok && own != nil {
			b.Env = own.Env
		}
		if b.Prompt == "" {
			b.Prompt = promptText
		}
		b.Files = nil // раннеру файлы набора не нужны
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil, errors.New("в наборе инстанса нет привязок: сервер прежней версии")
	}
	return out, nil
}

// loadBindings — привязки инстанса, а не вышло — встроенные с
// предупреждением.
func loadBindings(ctx context.Context, apiURL string, log *slog.Logger) ([]agentkit.Binding, string, error) {
	builtin, err := builtinBindings()
	if err != nil {
		return nil, "", err
	}
	bindings, err := instanceBindings(ctx, apiURL, builtin)
	if err != nil {
		log.Warn("набор агента инстанса недоступен: раннер берёт встроенные привязки", "error", err)
		return builtin, bindingsBuiltin, nil
	}
	return bindings, bindingsInstance, nil
}

func findBinding(bindings []agentkit.Binding, name string) *agentkit.Binding {
	for i := range bindings {
		if bindings[i].Name == name {
			return &bindings[i]
		}
	}
	return nil
}

// genericBinding — привязка своей команды неизвестного агента: без разбора
// вывода, лимита и окружения агента, инструкция текстом.
func genericBinding(bindings []agentkit.Binding) agentkit.Binding {
	if b := findBinding(bindings, launch.AgentGeneric); b != nil {
		return *b
	}
	return agentkit.Binding{Name: launch.AgentGeneric, Executor: "process", Prompt: promptText}
}

// selectAgent — привязка раннера (0077 п. 8). Своя команда — по имени её
// программы: привязка с таким именем, иначе generic (обёртка «env X=1
// claude …» — тоже generic: первое слово env). Без своей команды — по PATH
// среди привязок с шаблоном run: claude, если он есть (как до привязок),
// иначе ровно одна найденная; несколько без claude и ни одной — отказ с
// перечнем: какого агента запускать, раннер не угадывает.
func selectAgent(command string, bindings []agentkit.Binding, lookPath func(string) (string, error)) (agentkit.Binding, string, error) {
	if fields := strings.Fields(command); len(fields) > 0 {
		prog := launch.Program(fields[0])
		if b := findBinding(bindings, prog); b != nil && b.Name != launch.AgentGeneric {
			return *b, agentFromCommand, nil
		}
		return genericBinding(bindings), agentFromCommand, nil
	}
	var known, found []string
	for _, b := range bindings {
		if len(b.Run) == 0 {
			continue
		}
		known = append(known, b.Run[0])
		if _, err := lookPath(b.Run[0]); err == nil {
			found = append(found, b.Name)
		}
	}
	slices.Sort(known)
	slices.Sort(found)
	switch {
	case slices.Contains(found, launch.AgentClaude):
		return *findBinding(bindings, launch.AgentClaude), agentFromPath, nil
	case len(found) == 1:
		return *findBinding(bindings, found[0]), agentFromPath, nil
	case len(found) == 0:
		return agentkit.Binding{}, "", fmt.Errorf("в PATH нет ни одного агента из привязок (%s): задайте SDLC_RUNNER_COMMAND — команду запуска своего агента",
			strings.Join(known, ", "))
	}
	return agentkit.Binding{}, "", fmt.Errorf("в PATH несколько агентов без claude: %s — какого запускать, раннер не выбирает; задайте SDLC_RUNNER_COMMAND, например %s …",
		strings.Join(found, ", "), found[0])
}

// prepareAgent выбирает привязку до запуска очереди: отказ здесь — код 2 и
// ни одного захвата.
func prepareAgent(ctx context.Context, cfg *Config, log *slog.Logger, lookPath func(string) (string, error)) error {
	bindings, from, err := loadBindings(ctx, cfg.APIURL, log)
	if err != nil {
		return err
	}
	agent, agentFrom, err := selectAgent(cfg.Command, bindings, lookPath)
	if err != nil {
		return err
	}
	cfg.Agent, cfg.AgentFrom = &agent, agentFrom
	log.Info("агент раннера", "agent", agent.Name, "agent_from", agentFrom, "bindings", from,
		"output", dashOf(agent.Output), "prompt", agent.Prompt)
	return nil
}

// defaultAgent — привязка claude встроенного набора: раннер, собранный в
// обход main, работает как до привязок.
func defaultAgent() agentkit.Binding {
	if bindings, err := builtinBindings(); err == nil {
		if b := findBinding(bindings, launch.AgentClaude); b != nil {
			return *b
		}
	}
	return genericBinding(nil)
}

func dashOf(s *string) string {
	if s == nil || *s == "" {
		return "нет"
	}
	return *s
}

// output — формат вывода привязки; пусто — вывода раннер не разбирает.
func output(b agentkit.Binding) string {
	if b.Output == nil {
		return ""
	}
	return *b.Output
}

// levelsOf — таблица уровней привязки как map для launch.PickModel.
func levelsOf(b agentkit.Binding) map[string]string {
	if b.Levels == nil {
		return nil
	}
	return map[string]string{"low": b.Levels.Low, "medium": b.Levels.Medium, "high": b.Levels.High}
}

// hookPlacement — где привязка крепит хук-страховку; пусто — хука нет.
func hookPlacement(b agentkit.Binding) string {
	var h struct {
		Placement string `json:"placement"`
	}
	if len(b.Hook) == 0 || json.Unmarshal(b.Hook, &h) != nil {
		return ""
	}
	return h.Placement
}
