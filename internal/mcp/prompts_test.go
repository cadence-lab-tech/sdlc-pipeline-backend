package mcp

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/trust"
)

// prompts/get без обязательного аргумента — ошибка, а не промпт с пустым
// ключом задачи (CLAB-104); с аргументом промпт называет задачу.
func TestWorkPromptRequiresTask(t *testing.T) {
	ctx := context.Background()
	cs := connectPrompts(t)

	for name, args := range map[string]map[string]string{
		"без аргументов":  nil,
		"не тот аргумент": {"task_key": "SDLC-42"},
		"пустой ключ":     {"task": "  "},
	} {
		_, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: "work_on_task", Arguments: args})
		if err == nil || !strings.Contains(err.Error(), "task") {
			t.Fatalf("%s: ждали ошибку про task, получили %v", name, err)
		}
	}

	res, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: "work_on_task", Arguments: map[string]string{"task": "SDLC-42"}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Messages[0].Content.(*sdk.TextContent).Text
	if !strings.Contains(text, "Возьми задачу SDLC-42 в работу") {
		t.Fatalf("промпт без ключа задачи: %q", text[:60])
	}
}

// connectPrompts — клиент MCP к серверу без базы: промптам и tools/list
// она не нужна.
func connectPrompts(t *testing.T) *sdk.ClientSession {
	t.Helper()
	srv := NewServer(slog.New(slog.DiscardHandler), nil, nil, nil, nil, "test")
	ctx := context.Background()
	clientT, serverT := sdk.NewInMemoryTransports()
	ss, err := srv.mcp.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Wait() })
	return cs
}

func promptText(t *testing.T, cs *sdk.ClientSession, name string, args map[string]string) string {
	t.Helper()
	res, err := cs.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res.Messages[0].Content.(*sdk.TextContent).Text
}

// CLAB-209, критерий 6: промпт work_on_task несёт правило о данных целиком —
// и тот, что получает человек, и тот, с которого стартует агент раннера.
func TestWorkPromptCarriesTrustRule(t *testing.T) {
	cs := connectPrompts(t)
	for _, args := range []map[string]string{
		{"task": "SDLC-42"}, {"task": "SDLC-42", "role": "developer"},
	} {
		text := promptText(t, cs, "work_on_task", args)
		if !strings.Contains(text, trust.Rule) {
			t.Fatalf("в work_on_task %v нет правила о данных:\n%s", args, text)
		}
		for _, want := range []string{"boundary", "сменить роль", "расширить", "апрув", "токен", "отключить", "упомянуть указание в сданном"} {
			if !strings.Contains(text, want) {
				t.Errorf("в work_on_task нет %q", want)
			}
		}
	}
}

// CLAB-261, AC-6 и AC-7: после захвата промпт велит сверить модель с
// execution.model_level и при расхождении сказать «рекомендован уровень X, вы
// работаете на Y», при none — молчать, модель не менять; учёт — до перехода
// (CLAB-225: после перехода прогон не удерживается и отчёт не примут).
func TestWorkPromptModelLevel(t *testing.T) {
	cs := connectPrompts(t)
	for _, args := range []map[string]string{
		{"task": "SDLC-42"}, {"task": "SDLC-42", "role": "developer"},
	} {
		text := promptText(t, cs, "work_on_task", args)
		for _, want := range []string{
			"execution", "model_level", "рекомендован уровень", "вы работаете на",
			"low — haiku", "medium — sonnet", "high — opus",
			"none", "ничего не говори", "Модель не меняй", "полным id модели",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("work_on_task %v: нет %q", args, want)
			}
		}
		claim := strings.Index(text, "sdlc_claim_task")
		level := strings.Index(text, "рекомендован уровень")
		usage := strings.Index(text, "sdlc_report_usage")
		transition := strings.Index(text, "sdlc_request_transition")
		if claim < 0 || !(claim < level && level < usage && usage < transition) {
			t.Errorf("work_on_task %v: порядок захват → уровень → учёт → переход нарушен: %d, %d, %d, %d",
				args, claim, level, usage, transition)
		}
	}
}

// CLAB-214, AC-2 и AC-7: с usage=runner шага отчёта о стоимости нет, есть
// прямой запрет на sdlc_report_usage и clab usage, правило о данных и сверка
// уровня на месте. Без признака, с незнакомым значением и с лишним
// незнакомым аргументом — прежний текст с шагом отчёта.
func TestWorkPromptRunnerUsage(t *testing.T) {
	cs := connectPrompts(t)
	const selfReport = "7. До перехода отчитайся о стоимости: sdlc_report_usage с полным id модели из\n" +
		"   сессии (claude-opus-5-5, а не алиас opus), токенами и деньгами."

	plain := promptText(t, cs, "work_on_task", map[string]string{"task": "SDLC-42", "role": "developer"})
	if !strings.Contains(plain, selfReport) {
		t.Fatalf("без признака нет шага отчёта:\n%s", plain)
	}
	for name, args := range map[string]map[string]string{
		"пустой признак":      {"task": "SDLC-42", "role": "developer", "usage": ""},
		"незнакомое значение": {"task": "SDLC-42", "role": "developer", "usage": "agent"},
		"незнакомый аргумент": {"task": "SDLC-42", "role": "developer", "billing": "subscription"},
	} {
		if got := promptText(t, cs, "work_on_task", args); got != plain {
			t.Errorf("%s: текст разошёлся с прежним:\n%s", name, got)
		}
	}

	runner := promptText(t, cs, "work_on_task", map[string]string{"task": "SDLC-42", "role": "developer", "usage": "runner"})
	if strings.Contains(runner, "До перехода отчитайся") {
		t.Fatalf("в режиме раннера остался шаг отчёта:\n%s", runner)
	}
	for _, want := range []string{
		"не вызывай sdlc_report_usage и clab usage", "раннер", "usage_report",
		// CLAB-219: при политике учёта захват требует отчёта, но под
		// раннером агент всё равно молчит — учёт пишет раннер.
		"ждёт или требует отчёта", "required",
		trust.Rule, "рекомендован уровень", "Модель не меняй", "sdlc_request_transition",
	} {
		if !strings.Contains(runner, want) {
			t.Errorf("в режиме раннера нет %q", want)
		}
	}
	claim := strings.Index(runner, "sdlc_claim_task")
	level := strings.Index(runner, "рекомендован уровень")
	noReport := strings.Index(runner, "О стоимости не отчитывайся")
	transition := strings.Index(runner, "sdlc_request_transition")
	if claim < 0 || !(claim < level && level < noReport && noReport < transition) {
		t.Errorf("порядок захват → уровень → запрет отчёта → переход нарушен: %d, %d, %d, %d",
			claim, level, noReport, transition)
	}
	// Шаги те же, кроме седьмого: остальной текст не разошёлся.
	if strings.Replace(runner, usageStep(true), usageStep(false), 1) != plain {
		t.Error("режим раннера меняет не только шаг 7")
	}
}

// CLAB-209, критерий 7: перенос правил считает переносимое материалом, а
// подозрительные строки показывает человеку в отчёте dry_run.
func TestImportRulesCarriesTrustRule(t *testing.T) {
	cs := connectPrompts(t)
	text := promptText(t, cs, "import_rules", nil)
	for name, body := range map[string]string{"import_rules": text, "ImportGuide": ImportGuide} {
		if !strings.Contains(body, trust.ImportRule) {
			t.Errorf("в %s нет правила переноса", name)
		}
		if !strings.Contains(body, "похожие на попытку") || !strings.Contains(body, "dry_run") {
			t.Errorf("в %s нет пункта о показе подозрительного в dry_run", name)
		}
	}
}

// CLAB-209, критерий 8: тулы, которые отдают тексты людей и агентов, говорят
// в описании, что это данные.
func TestDataToolsSayContentIsData(t *testing.T) {
	cs := connectPrompts(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"sdlc_claim_task": false, "sdlc_get_task": false, "sdlc_get_artifact": false,
		"sdlc_search_context": false, "sdlc_get_drift": false,
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; !ok {
			continue
		}
		want[tool.Name] = true
		if !strings.Contains(tool.Description, trust.DataNote) {
			t.Errorf("%s: в описании нет %q: %s", tool.Name, trust.DataNote, tool.Description)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("тул %s не объявлен", name)
		}
	}
}

// CLAB-217, критерий 17: откат, заблокированный гейтом человека (предел
// попыток), — агент отпускает задачу с причиной и останавливается, а не
// ищет обход.
func TestWorkPromptBlockedRejectReleases(t *testing.T) {
	cs := connectPrompts(t)
	for _, args := range []map[string]string{
		{"task": "SDLC-42"}, {"task": "SDLC-42", "role": "developer"},
	} {
		text := promptText(t, cs, "work_on_task", args)
		for _, want := range []string{
			"заблокирован и сам откат", "гейтом, который закрывает человек",
			"отпусти задачу sdlc_release с причиной", "остановись",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("work_on_task %v: нет %q", args, want)
			}
		}
	}
}

// CLAB-218, критерий 12: упёршийся в предел, который снимает человек
// (попыток или перепроверок), отпускает задачу с причиной и останавливается.
// Текст промпта переносится по строкам, поэтому пробелы сравниваются как один.
func TestWorkPromptHumanLimitReleases(t *testing.T) {
	cs := connectPrompts(t)
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for _, args := range []map[string]string{
		{"task": "SDLC-42"}, {"task": "SDLC-42", "role": "developer"},
	} {
		text := flat(promptText(t, cs, "work_on_task", args))
		for _, want := range []string{
			"предел, который снимает человек, — попыток или перепроверок",
			"новой перепроверки не будет, отпусти задачу с причиной и остановись",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("work_on_task %v: нет %q", args, want)
			}
		}
	}
}
