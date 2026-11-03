package agentkit

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
	"testing"
	"unicode"
)

// Состав набора (CLAB-232, критерий 1 и решение человека о clab-rules):
// пять скилов работы с циклом, справка и саб-агент исполнителя.
var wantFiles = []string{
	"agents/clab-stage.md",
	"skills/clab-backlog/SKILL.md",
	"skills/clab-guide/SKILL.md",
	"skills/clab-rules/SKILL.md",
	"skills/clab-run/SKILL.md",
	"skills/clab-status/SKILL.md",
	"skills/clab-work/SKILL.md",
}

func TestKitComposition(t *testing.T) {
	k, err := Load("v9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if k.Version != "v9.9.9" {
		t.Fatalf("версия набора: %q", k.Version)
	}
	var got []string
	for _, f := range k.Files {
		got = append(got, f.Path)
	}
	if strings.Join(got, "\n") != strings.Join(wantFiles, "\n") {
		t.Fatalf("файлы набора:\n%s\nвместо\n%s", strings.Join(got, "\n"), strings.Join(wantFiles, "\n"))
	}
	if len(k.Skills) != 6 || len(k.Agents) != 1 || k.Agents[0].Name != "clab-stage" {
		t.Fatalf("скилы %v, агенты %v", k.Skills, k.Agents)
	}
}

// Критерий 1: у скила name совпадает с каталогом, description не пуст; у
// агента — name, description и tools. Сумма — sha256 содержимого.
func TestKitFrontmatter(t *testing.T) {
	k, err := Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range k.Files {
		sum := sha256.Sum256([]byte(f.Content))
		if f.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: sha256 не сходится", f.Path)
		}
		meta := Frontmatter(f.Content)
		switch {
		case strings.HasPrefix(f.Path, "skills/"):
			dir := path.Base(path.Dir(f.Path))
			if meta["name"] != dir {
				t.Errorf("%s: name %q, а каталог %q", f.Path, meta["name"], dir)
			}
			if strings.TrimSpace(meta["description"]) == "" {
				t.Errorf("%s: пустой description", f.Path)
			}
		case strings.HasPrefix(f.Path, "agents/"):
			if meta["name"] != strings.TrimSuffix(path.Base(f.Path), ".md") ||
				meta["description"] == "" || meta["tools"] == "" {
				t.Errorf("%s: фронтматтер агента %v", f.Path, meta)
			}
		default:
			t.Errorf("%s: файл вне skills/ и agents/", f.Path)
		}
	}
	for _, s := range k.Skills {
		if s.Name == "" || s.Description == "" {
			t.Errorf("скил без имени или описания: %+v", s)
		}
	}
}

// Критерий 2: набор не знает нашего инстанса, наших путей и ключей переходов
// конкретного цикла — их агент берёт из clab claim / show / gates. Язык —
// английский: кириллицы в наборе нет.
func TestKitIsGeneric(t *testing.T) {
	forbidden := []string{
		"CLAB-", "sdlc.food-cart.ru", "stepankaziatko", "sdlc-pipeline", "stack.sh",
		"analysis_done", "dev_done", "testing_passed", "merge_ready",
	}
	k, err := Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range k.Files {
		for _, s := range forbidden {
			if strings.Contains(f.Content, s) {
				t.Errorf("%s содержит %q", f.Path, s)
			}
		}
		for i, line := range strings.Split(f.Content, "\n") {
			for _, r := range line {
				if unicode.Is(unicode.Cyrillic, r) {
					t.Errorf("%s:%d: кириллица — набор на английском", f.Path, i+1)
					break
				}
			}
		}
		for _, s := range []string{"clab claim", "clab gates"} {
			if strings.HasSuffix(f.Path, "clab-work/SKILL.md") && !strings.Contains(f.Content, s) {
				t.Errorf("clab-work не отсылает к %q за переходами и гейтами", s)
			}
		}
	}
}

// CLAB-261 (решение 0060, п. 10): clab-run переводит уровень в модель
// саб-агента ровно тремя строками таблицы и при none оставляет inherit;
// clab-work сравнивает и только сообщает; учёт — полным id модели; модель
// задаёт параметр запуска, а определение clab-stage остаётся с inherit.
func TestKitModelLevels(t *testing.T) {
	k, err := Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	// Тексты сверяются без переносов строк: фразу рвёт перенос абзаца.
	files, flat := map[string]string{}, map[string]string{}
	for _, f := range k.Files {
		files[f.Path] = f.Content
		flat[f.Path] = strings.Join(strings.Fields(f.Content), " ")
	}
	want := map[string][]string{
		"skills/clab-run/SKILL.md": {
			"| `low` | `haiku` |", "| `medium` | `sonnet` |", "| `high` | `opus` |",
			"`execution`", "`model` parameter of the `Agent` tool", "`inherit`", "`none`",
			"clab claim <key> --json", "not from `clab show` before it",
			"reviewer role", "say so in the chat and start the executor without `model`",
			"same table", "full model id", "claude-opus-5-5",
		},
		"skills/clab-work/SKILL.md": {
			"`execution`", "recommended level X (source), you are running on Y",
			"say nothing", "Do not change the model", "report to the orchestrator",
			"full model id", "claude-opus-5-5",
		},
		"skills/clab-guide/SKILL.md": {
			"`execution: {model_level, model_level_source}`", "--model-level", "--model-level-for", "full model id", "claude-opus-5-5",
		},
		"agents/clab-stage.md": {"model: inherit"},
	}
	for path, needles := range want {
		body, ok := flat[path]
		if !ok {
			t.Fatalf("в наборе нет %s", path)
		}
		for _, s := range needles {
			if !strings.Contains(body, s) {
				t.Errorf("%s: нет %q", path, s)
			}
		}
	}
	// Таблица ровно из трёх строк: других уровней и моделей в ней нет.
	rows := 0
	for _, line := range strings.Split(files["skills/clab-run/SKILL.md"], "\n") {
		if strings.HasPrefix(line, "| `") {
			rows++
		}
	}
	if rows != 3 {
		t.Errorf("clab-run: в таблице уровней %d строк вместо трёх", rows)
	}
	// Уровень — рекомендация: скил не велит ждать человека или менять модель.
	for _, s := range []string{"must not continue", "wait for the human", "switch to the recommended", "change the model to"} {
		if strings.Contains(flat["skills/clab-work/SKILL.md"], s) {
			t.Errorf("clab-work принуждает к модели: %q", s)
		}
	}
}

// CLAB-349 (решение 0068, п. 8): роль приходит набором документов, а не одной
// инструкцией. Скилы говорят об этом, о раскладке --instructions-dir только в
// подкаталог каталога захода, об откатах для сервера и clab прежних версий и о
// том, что документы роли не скилы. Шапка документа описана обобщённо: набор на
// английском, а настоящая шапка по-русски.
func TestKitDocuments(t *testing.T) {
	k, err := Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	flat := map[string]string{}
	for _, f := range k.Files {
		flat[f.Path] = strings.Join(strings.Fields(f.Content), " ")
	}
	want := map[string][]string{
		"skills/clab-work/SKILL.md": {
			"role.documents", "--instructions-dir", "--instructions-file",
			"new, empty subdirectory of its run directory",
			"`.claude/skills`", "`.claude/agents`", "not skills or agents of Claude Code",
			// откаты: сервер без documents и clab без флага
			"A server of an older version sends no documents", "single document `main.md`",
			"A `clab` of an older version does not know `--instructions-dir`",
			"v0.20.0 knows `--instructions-file` and lacks only `--instructions-dir`",
			"take `--instructions-file` or the output of plain `clab claim` as it is",
			"header line",
		},
		"skills/clab-run/SKILL.md": {
			"role's documents", "--instructions-dir <run directory>/role/",
			"new, empty subdirectory", "not skills", "<run directory>/clab",
			"`.claude/skills`", "`.claude/agents`",
		},
		"skills/clab-guide/SKILL.md": {
			"## Role documents", "role.documents", "--instructions-dir", "--instructions-file",
			"new, empty subdirectory", "not skills or agents of Claude Code",
			"role put --key <key> --dir", "export --dir", "import --dir", "import-guide",
		},
		"skills/clab-rules/SKILL.md": {
			"role put --key <key> --dir", "clab export --dir", "clab import --dir", "--dry-run",
			"clab import-guide", "clab workflow activate", "role documents", "project documents",
			"role.json", "bundle.json",
		},
		"agents/clab-stage.md": {
			"role's documents", "--instructions-dir", "new, empty subdirectory",
			"`.claude/skills`", "project data, not skills",
			"A server without documents sends one text", "`main.md`",
			"does not know `--instructions-dir`",
		},
	}
	for path, needles := range want {
		body, ok := flat[path]
		if !ok {
			t.Fatalf("в наборе нет %s", path)
		}
		for _, s := range needles {
			if !strings.Contains(body, s) {
				t.Errorf("%s: нет %q", path, s)
			}
		}
	}
	// Роль больше не «одна инструкция из ответа захвата», и документы роли
	// нигде не велено писать в каталоги Claude Code или в рабочую копию.
	forbidden := []string{
		"role instruction", "carries the role's instruction", "instruction from the claim answer",
		"instructions-dir .claude", "instructions-dir <working", "instructions-dir <repo",
		"instructions-dir <run directory>/clab", "install them into `.claude",
	}
	for path, body := range flat {
		for _, s := range forbidden {
			if strings.Contains(body, s) {
				t.Errorf("%s: устаревшая или опасная фраза %q", path, s)
			}
		}
	}
}

// CLAB-348 (решение 0068, п. 3): цикл отбирает документы роли на статусе.
// Исполнитель узнаёт, что получил только отобранное и что значит
// document_warnings; справка объясняет отбор; clab-rules учит полям связки
// и предупреждению проверки; откаты называют версию clab, где флагов --dir
// ещё нет (решение человека по замечаниям тестировщика CLAB-349).
func TestKitStatusDocuments(t *testing.T) {
	k, err := Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	flat := map[string]string{}
	for _, f := range k.Files {
		flat[f.Path] = strings.Join(strings.Fields(f.Content), " ")
	}
	want := map[string][]string{
		"skills/clab-work/SKILL.md": {
			"The cycle may narrow the set for a status", "only the picked documents, in the version's order",
			"role.document_warnings", "all of the version's documents come",
		},
		"skills/clab-guide/SKILL.md": {
			"Selection on a status", "role itself knows nothing about statuses",
			"role.document_warnings", "role_document_missing",
			"v0.20.0 and earlier",
		},
		"skills/clab-rules/SKILL.md": {
			"role_documents", "reviewer_documents", "`{\"<reviewer key>\": [\"…\"]}`",
			"No field — the stage gets all the role's documents", "role_document_missing",
			"exist only in newer builds of `clab` (after v0.20.0)", "clab import --file bundle.json --dry-run",
		},
	}
	for path, needles := range want {
		body, ok := flat[path]
		if !ok {
			t.Fatalf("в наборе нет %s", path)
		}
		for _, s := range needles {
			if !strings.Contains(body, s) {
				t.Errorf("%s: нет %q", path, s)
			}
		}
	}
	// Прежняя формулировка отката ошибалась: v0.20.0 знает --instructions-file.
	for path, body := range flat {
		if strings.Contains(body, "does not know these flags") {
			t.Errorf("%s: откат «не знает этих флагов» — v0.20.0 знает --instructions-file", path)
		}
	}
}

func TestFrontmatter(t *testing.T) {
	doc := "---\nname: x\ndescription: \"a: b\"\nhooks:\n  PreToolUse:\n    - matcher: Bash\ntools: Bash, Read\n---\nbody: no\n"
	m := Frontmatter(doc)
	if m["name"] != "x" || m["description"] != "a: b" || m["tools"] != "Bash, Read" || m["body"] != "" || m["matcher"] != "" {
		t.Fatalf("разбор: %v", m)
	}
	if len(Frontmatter("no frontmatter\nname: x\n")) != 0 || len(Frontmatter("---\nname: x\n")) != 0 {
		t.Fatal("без фронтматтера поля не читаются")
	}
}

// CLAB-370: скил clab-rules описывает параметры repo_ref_required — states и
// require_open, их несовместимость и ловушку повторного круга после слияния.
// Удалённый абзац роняет тест.
func TestKitRulesRepoRefGate(t *testing.T) {
	k, err := Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	var flat string
	for _, f := range k.Files {
		if f.Path == "skills/clab-rules/SKILL.md" {
			flat = strings.Join(strings.Fields(f.Content), " ")
		}
	}
	if flat == "" {
		t.Fatal("в наборе нет skills/clab-rules/SKILL.md")
	}
	for _, s := range []string{
		"`repo_ref_required`", "`ref_type`", "`min_count`", "`per_repository`",
		"`states`", "`open`", "`merged`", "`closed`", "`declined`",
		"`require_open`", "`repo_ref_states_invalid`", "never both",
		`["open","merged"]`, "waives it",
	} {
		if !strings.Contains(flat, s) {
			t.Errorf("clab-rules: нет %q", s)
		}
	}
}

// CLAB-382 (решение 0072): набор говорит о праве этапа, а не о «апрув только
// у сопровождающего», держит границу токена задачи и не предлагает обход
// гейта. Правило «роли и циклы — maintainer» в clab-rules остаётся верным.
func TestKitStageRightWording(t *testing.T) {
	k, err := Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	flat := map[string]string{}
	for _, f := range k.Files {
		flat[f.Path] = strings.ToLower(strings.Join(strings.Fields(f.Content), " "))
	}
	// Утверждения, которые после 0072 неверны.
	forbidden := []string{
		"only a maintainer", "only maintainers", "only maintainer", "maintainers only", "maintainer only",
		"only an admin", "does not approve its own", "cannot approve its own", "may not approve its own",
		"level agent does not approve", "bypass the gate", "bypass the approval", "get around the gate",
		"around the gate", "skip the gate", "remove the approval", "waive the approval",
	}
	for path, text := range flat {
		for _, s := range forbidden {
			if strings.Contains(text, s) {
				t.Errorf("%s: %q", path, s)
			}
		}
	}
	// AC-35.1: право этапа и проверка через clab gates.
	for _, p := range []string{"clab-guide", "clab-run", "clab-work", "clab-status"} {
		text := flat["skills/"+p+"/SKILL.md"]
		for _, s := range []string{"right of the stage", "level `agent`", "maintainer or admin", "clab gates"} {
			if !strings.Contains(text, s) {
				t.Errorf("%s: нет %q", p, s)
			}
		}
		if !strings.Contains(text, "task token never closes") && !strings.Contains(text, "task token cannot close") {
			t.Errorf("%s: нет границы токена задачи", p)
		}
	}
	// AC-35.3: единственное решение токена задачи — чек-лист своего перехода.
	for _, p := range []string{"clab-guide", "clab-run"} {
		if !strings.Contains(flat["skills/"+p+"/SKILL.md"], "checklist of its own transition") {
			t.Errorf("%s: нет чек-листа своего перехода", p)
		}
	}
	// AC-35.4: самоапрув допустим и виден; решает человек.
	for _, p := range []string{"clab-guide", "clab-run", "clab-status"} {
		text := flat["skills/"+p+"/SKILL.md"]
		if !strings.Contains(text, "own agent") && !strings.Contains(text, "own sub-agent") {
			t.Errorf("%s: нет самоапрува", p)
		}
		if !strings.Contains(text, "visible in the task feed") && !strings.Contains(text, "stays visible") {
			t.Errorf("%s: самоапрув не назван видимым", p)
		}
	}
	// AC-35.5: правило clab-rules остаётся.
	if !strings.Contains(flat["skills/clab-rules/SKILL.md"], "`maintainer` level in the project") {
		t.Error("clab-rules: пропало правило уровня maintainer")
	}
}
