package main

// Стартовый проект установки (CLAB-199): проект в команде, связка
// bundles/starter.json пакета и активные циклы — шаг, который администратор
// раньше проходил руками по docs/admin.md. Его зовут install --with-starter
// после готовности стека и server starter на уже установленной машине.
//
// Шаг не знает ни ролей, ни циклов связки: что заводить и что активировать,
// он берёт из самой связки и из отчёта импорта. Логику импорта и активации не
// повторяет — те же ручки, что у clab import и clab workflow activate.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

const starterHelp = `clab server starter — set up the starter project of this installation:
create the project in a team, apply bundles/starter.json of the package (its
roles and workflows) and activate the workflows. It finishes the step of
clab server install --with-starter, or adds the project to an installation
made without it.

Usage: clab server starter --project KEY [--dir DIR] [--team KEY]
                           [--project-name NAME]

  clab server starter --dir /opt/cadence --project ACME
  clab server starter --project PLAT --team platform --project-name Platform

Options:
  --dir DIR            the installation directory (clab server --help)
  --project KEY        the key of the project: 2-16 characters A-Z, 0-9 and _,
                       starting with a letter
  --project-name NAME  its display name, default the key
  --team KEY           the team that owns the project, default
                       SDLC_BOOTSTRAP_TEAM in .env

It talks to the API of this installation only, through its Caddy on this
machine (proxy mode: CADENCE_HTTP_ADDR; tls mode: 127.0.0.1:443 with the
certificate of SDLC_DOMAIN checked as usual), with the administrator token
of the installation (SDLC_ADMIN_TOKEN or SDLC_ADMIN_TOKEN_FILE). Neither a
sign-in nor the clab configuration is used, and the token is neither printed
nor stored.

Running it again changes nothing: the project exists, the roles and
workflows are unchanged, active workflows stay active, and drafts of the
starter workflows are activated. A project with rules of its own (another
instruction of a role, or another active workflow for the same task type)
is refused without changes; compare by hand with
clab import --project KEY --file bundles/starter.json --dry-run.

  CADENCE_READY_TIMEOUT  seconds to wait for the API (in tls mode, for the
                         certificate), default 300`

// starterBundlePath — связка стартового проекта внутри пакета.
const starterBundlePath = "bundles/starter.json"

var projectKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,15}$`)

// checkProjectKey — правило ключа из схемы (projects.key): отказ до записи,
// а не 400 от API после старта стека.
func checkProjectKey(key string) error {
	if key == "" {
		return errors.New("the starter project needs a key: --project KEY (2-16 characters A-Z, 0-9 and _, starting with a letter)")
	}
	if !projectKeyPattern.MatchString(key) {
		return fmt.Errorf("'%s' is not a project key: 2-16 characters A-Z, 0-9 and _, starting with a letter (ACME, PLATFORM_2)", key)
	}
	return nil
}

// starterTarget — где заводится стартовый проект.
type starterTarget struct {
	team, key, name string
}

// starterBundle — связка как есть (уходит в API байт в байт) и то, что шагу
// нужно знать о ней: ключи ролей и типы задач циклов.
type starterBundle struct {
	raw       []byte
	roles     []string
	workflows []starterWorkflowDef
}

type starterWorkflowDef struct {
	name      string
	taskTypes []string
}

// loadStarterBundle читает bundles/starter.json пакета в каталоге dir; what —
// чей это пакет для сообщения (временный каталог install человеку ничего не
// скажет).
func loadStarterBundle(dir, what string) (*starterBundle, error) {
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(starterBundlePath)))
	if err != nil {
		return nil, fmt.Errorf("%s has no %s, which the starter project is made of; set the project up by hand (docs/admin.md, \"First cycle and roles\")", what, starterBundlePath)
	}
	return parseStarterBundle(raw, starterBundlePath+" of "+what)
}

func parseStarterBundle(raw []byte, where string) (*starterBundle, error) {
	var b struct {
		Roles []struct {
			Key string `json:"key"`
		} `json:"roles"`
		Workflow  *json.RawMessage `json:"workflow"`
		Workflows []struct {
			Name      string   `json:"name"`
			TaskTypes []string `json:"task_types"`
		} `json:"workflows"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("%s is not a JSON bundle: %v", where, err)
	}
	out := &starterBundle{raw: raw}
	for _, r := range b.Roles {
		out.roles = append(out.roles, r.Key)
	}
	for _, w := range b.Workflows {
		out.workflows = append(out.workflows, starterWorkflowDef{name: w.Name, taskTypes: w.TaskTypes})
	}
	if b.Workflow != nil && len(b.Workflows) == 0 {
		// Связка первой версии: один цикл полем workflow (Bundle.workflows).
		var w struct {
			Name      string   `json:"name"`
			TaskTypes []string `json:"task_types"`
		}
		if err := json.Unmarshal(*b.Workflow, &w); err != nil {
			return nil, fmt.Errorf("%s is not a JSON bundle: %v", where, err)
		}
		out.workflows = append(out.workflows, starterWorkflowDef{name: w.Name, taskTypes: w.TaskTypes})
	}
	if len(out.workflows) == 0 {
		return nil, fmt.Errorf("%s is not a bundle of roles and workflows: it has no workflows", where)
	}
	return out, nil
}

// types — типы задач цикла; без них цикл ведёт тип по умолчанию, как у
// сервера.
func (w starterWorkflowDef) types() []string {
	if len(w.taskTypes) == 0 {
		return []string{domain.DefaultTaskType}
	}
	return w.taskTypes
}

// ── подкоманда ──────────────────────────────────────────────────────────────

func serverStarter(ctx context.Context, e *serverEnv, args []string) error {
	var dir, team, key, name string
	pos, err := parseServerArgs(args, nil, map[string]*string{
		"--dir": &dir, "--team": &team, "--project": &key, "--project-name": &name,
	})
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError{fmt.Sprintf("unexpected argument: %s", pos[0])}
	}
	inst, err := openInstallation(e, dir)
	if err != nil {
		return err
	}
	if err := inst.open(ctx); err != nil {
		return err
	}
	if err := inst.checkMode(); err != nil {
		return err
	}
	if team == "" {
		team = inst.get("SDLC_BOOTSTRAP_TEAM")
	}
	if team == "" {
		return errors.New("the starter project needs a team: --team KEY (SDLC_BOOTSTRAP_TEAM in .env is empty)")
	}
	if err := checkProjectKey(key); err != nil {
		return err
	}
	if name == "" {
		name = key
	}
	bundle, err := loadStarterBundle(inst.dir, "the installation in "+inst.dir)
	if err != nil {
		return err
	}
	token, err := inst.secretSource().Value("SDLC_ADMIN_TOKEN")
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("the installation has no administrator token: SDLC_ADMIN_TOKEN and SDLC_ADMIN_TOKEN_FILE are empty in .env.\n" +
			"  Set the project up by hand with the token of an administrator: docs/admin.md, \"First cycle and roles\".")
	}
	return runStarter(ctx, inst, token, bundle, starterTarget{team: team, key: key, name: name}, true)
}

// starterRetry — команда повторить шаг с теми же ключами: clab server
// starter, под обёрткой пакета — путём этого clab (CLAB-242).
func starterRetry(e *serverEnv, dir string, t starterTarget) string {
	cmd := "clab server starter"
	if e.wrapped() {
		cmd = shellQuote(e.self) + " server starter"
	}
	cmd += " --dir " + shellQuote(dir) + " --team " + shellQuote(t.team) + " --project " + t.key
	if t.name != "" && t.name != t.key {
		cmd += " --project-name " + shellQuote(t.name)
	}
	return cmd
}

// ── шаг ─────────────────────────────────────────────────────────────────────

// runStarter — шаг целиком: API установки, сверка версии, проект, связка,
// активация, итог. signIn — печатать ли строку входа: install печатает её
// сам выше.
func runStarter(ctx context.Context, i *installation, token string, b *starterBundle, t starterTarget, signIn bool) error {
	e := i.e
	api, err := installAPIFor(i, token)
	if err != nil {
		return err
	}
	timeout, err := i.readyTimeout()
	if err != nil {
		return err
	}
	e.info("setting up the starter project %s in team %s through %s", t.key, t.team, api.base)
	got, err := api.waitInstance(ctx, e, timeout)
	switch apiStatus(err) {
	case 0:
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("the API of the installation at %s refused the administrator token of the installation: %s", api.base, redactToken(err.Error(), token))
	default:
		return fmt.Errorf("the API of the installation at %s: %s", api.base, redactToken(err.Error(), token))
	}
	if err != nil {
		hint := ""
		if i.getDefault("CADENCE_MODE", "tls") == "tls" {
			hint = "\n  In tls mode Caddy gets the certificate of " + i.get("SDLC_DOMAIN") +
				" from Let's Encrypt: check that the domain points at this machine and that ports 80 and 443 are reachable."
		}
		return fmt.Errorf("the API of the installation at %s did not answer in %ds: %s%s", api.base, timeout, redactToken(err.Error(), token), hint)
	}
	if got.Version != i.version {
		return fmt.Errorf("the API at %s is version %s, but the installation in %s is %s: it is not the API of this installation", api.base, got.Version, i.dir, i.version)
	}
	res, err := applyStarter(ctx, api, b, t)
	if err != nil {
		return errors.New(redactToken(err.Error(), token))
	}
	printStarter(e, i, b, t, res, signIn)
	return nil
}

// starterResult — что шаг сделал.
type starterResult struct {
	created   bool
	team      string // команда-владелец, как её назвал API
	roles     []apiImportEntry
	workflows []starterWorkflowResult
}

type starterWorkflowResult struct {
	name, outcome, activation string
}

// applyStarter — проект, сухой импорт и правило «чужие правила не
// затираются», импорт, активация черновиков. Повтор — тот же путь: проект
// есть, импорт отвечает unchanged, активные циклы не трогаются.
func applyStarter(ctx context.Context, api *installAPI, b *starterBundle, t starterTarget) (*starterResult, error) {
	res := &starterResult{team: t.team}
	project, err := api.project(ctx, t.key)
	if err != nil {
		return nil, fmt.Errorf("could not read project %s: %v", t.key, err)
	}
	if project == nil {
		if _, err := api.createProject(ctx, t.key, t.name, t.team); err != nil {
			return nil, fmt.Errorf("could not create project %s in team %s: %v", t.key, t.team, err)
		}
		res.created = true
	} else if project.OwnerTeamKey != nil && *project.OwnerTeamKey != "" {
		res.team = *project.OwnerTeamKey
	}

	dry, err := api.importBundle(ctx, t.key, b.raw, true)
	if err != nil {
		return nil, fmt.Errorf("could not check %s against project %s: %v", starterBundlePath, t.key, err)
	}
	before, err := api.workflows(ctx, t.key)
	if err != nil {
		return nil, fmt.Errorf("could not list the workflows of project %s: %v", t.key, err)
	}
	if conflicts := starterConflicts(b, dry, before); len(conflicts) > 0 {
		return nil, fmt.Errorf("project %s already has rules of its own, and the starter bundle would change them:\n    - %s\n"+
			"  Nothing was changed. Compare and apply by hand: clab import --project %s --file %s --dry-run",
			t.key, strings.Join(conflicts, "\n    - "), t.key, starterBundlePath)
	}

	rep, err := api.importBundle(ctx, t.key, b.raw, false)
	if err != nil {
		return nil, fmt.Errorf("could not apply %s to project %s: %v", starterBundlePath, t.key, err)
	}
	res.roles = rep.Roles
	after, err := api.workflows(ctx, t.key)
	if err != nil {
		return nil, fmt.Errorf("could not list the workflows of project %s: %v", t.key, err)
	}
	for _, entry := range rep.Workflows {
		w := starterWorkflowResult{name: entry.Key, outcome: entry.Outcome, activation: "already active"}
		if wf := workflowByID(after, entry.ID); wf == nil || wf.State != "active" {
			if err := api.activate(ctx, entry.ID); err != nil {
				return nil, fmt.Errorf("could not activate workflow %s of project %s: %v", entry.Key, t.key, err)
			}
			w.activation = "activated"
		}
		res.workflows = append(res.workflows, w)
	}
	return res, nil
}

// starterConflicts — где связка изменила бы правила проекта: новая версия
// роли или цикл, который сместил бы активный цикл проекта того же типа.
// Пустой проект и проект, где связка уже применена, конфликтов не дают.
func starterConflicts(b *starterBundle, dry apiImportReport, workflows []apiWorkflow) []string {
	var out []string
	for _, r := range dry.Roles {
		if r.Outcome != "created" && r.Outcome != "unchanged" {
			out = append(out, fmt.Sprintf("role %s: the instruction differs (%s)", r.Key, r.Outcome))
		}
	}
	for n, entry := range dry.Workflows {
		def := starterWorkflowDef{name: entry.Key}
		if n < len(b.workflows) {
			def = b.workflows[n]
		}
		if wf := workflowByID(workflows, entry.ID); entry.Outcome == "unchanged" && wf != nil && wf.State == "active" {
			continue
		}
		for _, other := range workflows {
			if other.State != "active" || other.ID == entry.ID || !overlaps(other.TaskTypes, def.types()) {
				continue
			}
			out = append(out, fmt.Sprintf("workflow %s: the project already runs its own active workflow %s (version %d) for %s",
				entry.Key, other.Name, other.Version, strings.Join(other.TaskTypes, ", ")))
		}
	}
	return out
}

func workflowByID(list []apiWorkflow, id string) *apiWorkflow {
	for n := range list {
		if list[n].ID == id && id != "" {
			return &list[n]
		}
	}
	return nil
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// printStarter — итог шага и следующий шаг администратора.
func printStarter(e *serverEnv, i *installation, b *starterBundle, t starterTarget, res *starterResult, signIn bool) {
	if res.created {
		e.ok("project %s created in team %s", t.key, res.team)
	} else {
		e.ok("project %s already exists (team %s)", t.key, res.team)
	}
	var roles []string
	for _, r := range res.roles {
		roles = append(roles, r.Key+" "+r.Outcome)
	}
	if len(roles) > 0 {
		e.ok("roles: %s", strings.Join(roles, ", "))
	}
	var flows []string
	for _, w := range res.workflows {
		outcome := w.outcome
		if outcome == "draft_created" {
			outcome = "created"
		}
		flows = append(flows, fmt.Sprintf("%s %s, %s", w.name, outcome, w.activation))
	}
	e.ok("workflows: %s", strings.Join(flows, "; "))

	host := signInURL(i)
	e.say("")
	e.kv("  Project:", fmt.Sprintf("%s (team %s)", t.key, res.team), "  Open it:", host+"/p/"+t.key)
	e.say("")
	e.say("Next:")
	if signIn {
		e.say("  sign in with the administrator token: clab auth login --host %s --with-token", host)
	}
	e.say("  invite a lead of the team:          clab invite create --team %s --role lead", res.team)
	if len(b.roles) > 0 {
		e.say("  invite people into the project:     clab invite create --project %s --level agent --roles %s", t.key, strings.Join(b.roles, ","))
	}
	e.say("  give the team access to it:         clab team grant %s --project %s --level agent", res.team, t.key)
	e.say("  create the first task:              clab create --project %s --title \"First task\"", t.key)
}
