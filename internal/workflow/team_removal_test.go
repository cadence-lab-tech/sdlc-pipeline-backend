package workflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Удаление арендатора необратимо и уносит его проекты целиком (CLAB-13).
// Проверяется, что уходит, что остаётся у соседей и что право на это — у лида.

// teamScopes — как посчитать строки команды в таблице. Список перечислен
// целиком намеренно: новая таблица со ссылкой на команду, забытая в удалении,
// иначе всплывёт только на проде — на projects и github_installations стоит
// RESTRICT, и забытая строка обвалит ручку внешним ключом.
var teamScopes = map[string]string{
	"projects":             `SELECT count(*) FROM projects WHERE owner_team_id = '%s'`,
	"team_members":         `SELECT count(*) FROM team_members WHERE team_id = '%s'`,
	"project_teams":        `SELECT count(*) FROM project_teams WHERE team_id = '%s'`,
	"team_idp_groups":      `SELECT count(*) FROM team_idp_groups WHERE team_id = '%s'`,
	"team_budgets":         `SELECT count(*) FROM team_budgets WHERE team_id = '%s'`,
	"subscriptions":        `SELECT count(*) FROM subscriptions WHERE team_id = '%s'`,
	"invites":              `SELECT count(*) FROM invites WHERE team_id = '%s'`,
	"team_events":          `SELECT count(*) FROM team_events WHERE team_id = '%s'`,
	"github_installations": `SELECT count(*) FROM github_installations WHERE owner_team_id = '%s'`,
}

// teamContents — что лежит на команде прямо сейчас. Запросы независимые:
// счётчики ручки не должны сверяться сами с собой.
func teamContents(t *testing.T, ctx context.Context, store *postgres.Store, teamID uuid.UUID) map[string]int {
	t.Helper()
	out := map[string]int{}
	for table, query := range teamScopes {
		out[table] = countRows(t, ctx, store, fmt.Sprintf(query, teamID))
	}
	return out
}

// ownerTeamOfSeedProject — команда-владелец демонстрационного проекта: он
// заводится сидом вместе с ней, и удаление этой команды обязано унести проект.
func ownerTeamOfSeedProject(t *testing.T, ctx context.Context, store *postgres.Store) domain.Team {
	t.Helper()
	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	if project.OwnerTeamID == nil {
		t.Fatal("у демонстрационного проекта нет команды-владельца")
	}
	team, err := store.GetTeamByKey(ctx, store.DB(), *project.OwnerTeamKey)
	if err != nil {
		t.Fatal(err)
	}
	return team
}

// installOn записывает установку GitHub на команду и привязывает ею
// репозиторий: на owner_team_id стоит RESTRICT, и без явного удаления
// установки ручка падала бы на внешнем ключе.
func installOn(t *testing.T, ctx context.Context, store *postgres.Store, teamID, actor uuid.UUID, installationID int64, repoProject string) {
	t.Helper()
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO github_installations
		    (installation_id, account_login, account_id, account_type, owner_team_id, connected_by)
		VALUES ($1, $2, $1, 'Organization', $3, $4)`,
		installationID, fmt.Sprintf("acct-%d", installationID), teamID, actor); err != nil {
		t.Fatal(err)
	}
	if repoProject == "" {
		return
	}
	if _, err := store.DB().Exec(ctx, `
		UPDATE repositories SET installation_id = $1
		WHERE project_id = (SELECT id FROM projects WHERE key = $2)`,
		installationID, repoProject); err != nil {
		t.Fatal(err)
	}
}

// Удаление уносит команду со всеми её проектами и строками, не трогая ни
// соседнюю команду, ни блобы, ни принципалов, и называет цену действия.
func TestDeleteTeamTakesEverythingWithIt(t *testing.T) {
	ctx := context.Background()
	store, engine, _, auth, system := setupPhase4(t, AuthHeader)
	admin := Identity{Principal: system}

	owner := ownerTeamOfSeedProject(t, ctx, store)

	// Первый проект команды набит всем, что на него ссылается, — тем же
	// наполнением, что у удаления проекта.
	projectWithEverything(t, ctx, store, engine, system)
	if _, err := auth.SetMember(ctx, admin, "SDLC", "agent:bot", domain.MemberAgent, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO agent_tokens (principal_id, project_id, name, token_hash)
		SELECT $1, id, 'ci', 'hash-clab-13' FROM projects WHERE key = 'SDLC'`, system.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO invites (project_id, level, token_hash, created_by, expires_at)
		SELECT id, 'viewer', 'inv-clab-13-project', $1, now() + interval '7 days'
		FROM projects WHERE key = 'SDLC'`, system.ID); err != nil {
		t.Fatal(err)
	}

	// Второй проект той же команды: удаление обязано взять оба, а не первый.
	second, err := engine.CreateProject(ctx, system.ID, postgres.CreateProjectInput{
		Key: "SECOND", Name: "Второй проект команды", OwnerTeamID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.AddRepository(ctx, "SECOND", AddRepositoryInput{
		Repository: "acme/tools", RoleHint: "tools"}); err != nil {
		t.Fatal(err)
	}

	// Соседняя команда со своим проектом, составом и установкой: она обязана
	// пережить удаление целиком.
	neighbourTeam, err := auth.CreateTeam(ctx, admin, CreateTeamInput{Key: "other", Name: "Соседи"})
	if err != nil {
		t.Fatal(err)
	}
	neighbour, err := engine.CreateProject(ctx, system.ID, postgres.CreateProjectInput{
		Key: "OTHER", Name: "Проект соседей", OwnerTeamID: neighbourTeam.ID})
	if err != nil {
		t.Fatal(err)
	}
	neighbourRepo, _, err := engine.AddRepository(ctx, "OTHER", AddRepositoryInput{
		Repository: "other/web", RoleHint: "web"})
	if err != nil {
		t.Fatal(err)
	}
	neighbourRun, _, err := store.EnqueueIndexRun(ctx, store.DB(), postgres.EnqueueIndexRunInput{
		RepositoryID: neighbourRepo.ID, Branch: "main", CommitSHA: strings.Repeat("b", 40), Trigger: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO code_files (index_run_id, repository_id, path, lang, size_bytes)
		VALUES ($1, $2, 'web.ts', 'ts', 10)`, neighbourRun.ID, neighbourRepo.ID); err != nil {
		t.Fatal(err)
	}
	alice := identityOf(t, store, "alice")
	bob := identityOf(t, store, "bob")
	if _, err := auth.SetTeamMember(ctx, admin, "other", "user:bob", domain.TeamRoleLead); err != nil {
		t.Fatal(err)
	}
	installOn(t, ctx, store, neighbourTeam.ID, system.ID, 4242, "OTHER")

	// Наполнение удаляемой команды: состав, тариф, бюджет, группы провайдера,
	// приглашение, лента аудита, установка и участие в двух проектах — своём и
	// чужом. Участие в чужом — то, что исчезнет, оставив проект живым.
	if _, err := auth.SetTeamMember(ctx, admin, owner.Key, "user:alice", domain.TeamRoleLead); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetTeamPlan(ctx, admin, owner.Key, "business"); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetTeamBudget(ctx, admin, owner.Key, "month", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetTeamIdPGroups(ctx, admin, owner.Key, []string{"sdlc-platform"}); err != nil {
		t.Fatal(err)
	}
	// Приглашение в команду — прямой вставкой: выдаёт его человек, а здесь
	// важна не выдача, а то, что строка уходит вместе с командой.
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO invites (team_id, team_role, token_hash, created_by, expires_at)
		VALUES ($1, 'member', 'inv-clab-13-team', $2, now() + interval '7 days')`,
		owner.ID, system.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetProjectTeam(ctx, admin, "SDLC", owner.Key, domain.MemberAdmin, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetProjectTeam(ctx, admin, "OTHER", owner.Key, domain.MemberMaintainer, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTeamEvent(ctx, store.DB(), owner.ID, domain.EventProjectDeleted, system.ID,
		map[string]any{"project_key": "GONE"}); err != nil {
		t.Fatal(err)
	}
	installOn(t, ctx, store, owner.ID, system.ID, 777, "SDLC")

	// Поимённое участие alice в чужом проекте: оно обязано пережить уход её
	// команды, в отличие от участия, полученного через команду.
	if _, err := auth.SetMember(ctx, admin, "OTHER", "user:alice", domain.MemberViewer, nil); err != nil {
		t.Fatal(err)
	}
	if role, err := store.EffectiveRole(ctx, store.DB(), neighbour.ID, alice.Principal.ID); err != nil ||
		role != domain.MemberMaintainer {
		t.Fatalf("до удаления alice в чужом проекте %q (%v), ожидали maintainer от команды", role, err)
	}
	// Инстансовый токен участника: он не привязан к проекту и не должен быть
	// отозван вместе с командой.
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO agent_tokens (principal_id, name, token_hash) VALUES ($1, 'личный', 'hash-clab-13-instance')`,
		alice.Principal.ID); err != nil {
		t.Fatal(err)
	}
	// Личная команда участника: она своя и уходом организации не задевается.
	personal, err := auth.PersonalTeam(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}

	neighbourBefore := teamContents(t, ctx, store, neighbourTeam.ID)
	blobsBefore := countRows(t, ctx, store, `SELECT count(*) FROM blobs`)
	principalsBefore := countRows(t, ctx, store, `SELECT count(*) FROM principals`)
	before := teamContents(t, ctx, store, owner.ID)
	firstProject, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	firstBefore := projectContents(t, ctx, store, firstProject.ID)

	deleted, counters, err := auth.DeleteTeam(ctx, admin, owner.Key)
	if err != nil {
		t.Fatalf("удаление команды: %v", err)
	}
	if deleted.ID != owner.ID {
		t.Fatalf("удалена не та команда: %s", deleted.ID)
	}

	// Счётчики сверяются со снимком, снятым до вызова, а не с подобранными
	// числами: ошибка на единицу иначе проходит незамеченной.
	for table, want := range before {
		if want == 0 {
			t.Fatalf("команда не набита: в %s не было ни строки", table)
		}
	}
	got := map[string]int{
		"projects":             counters.Projects,
		"team_members":         counters.Members,
		"invites":              counters.Invites,
		"github_installations": counters.Installations,
		"team_budgets":         counters.Budgets,
		"team_idp_groups":      counters.IdPGroups,
	}
	for table, want := range got {
		if want != before[table] {
			t.Fatalf("счётчик %s: в ответе %d, в базе до удаления было %d", table, want, before[table])
		}
	}
	// Гранты считаются только в чужих проектах: собственные уходят вместе с
	// проектом и попали бы в счёт дважды.
	if counters.ProjectGrants != 1 {
		t.Fatalf("грантов в чужих проектах %d, ожидали 1 (в OTHER)", counters.ProjectGrants)
	}
	if len(counters.ProjectKeys) != 2 ||
		!strings.Contains(strings.Join(counters.ProjectKeys, ","), "SDLC") ||
		!strings.Contains(strings.Join(counters.ProjectKeys, ","), "SECOND") {
		t.Fatalf("ключи удалённых проектов: %v", counters.ProjectKeys)
	}

	// Ни строки команды в базе не осталось.
	after := teamContents(t, ctx, store, owner.ID)
	for table, n := range after {
		if n != 0 {
			t.Fatalf("после удаления в %s осталось %d строк команды", table, n)
		}
	}
	if _, err := store.GetTeamByKey(ctx, store.DB(), owner.Key); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("команда всё ещё находится: %v", err)
	}

	// Содержимое её проектов ушло целиком: считается вся таблица, остаться
	// могут лишь строки проекта соседей. Инстансовые токены из счёта выведены
	// — они не принадлежат ни одному проекту и проверяются отдельно ниже.
	for _, table := range projectOwnedTables {
		query := fmt.Sprintf(`SELECT count(*) FROM %s`, table)
		if table == "agent_tokens" {
			query += ` WHERE project_id IS NOT NULL`
		}
		n := countRows(t, ctx, store, query)
		if n != 0 && !neighbourOwns(t, ctx, store, table, neighbour.ID, n) {
			t.Fatalf("после удаления команды в %s осталось %d строк", table, n)
		}
	}
	for table, want := range firstBefore {
		if want == 0 {
			t.Fatalf("проект команды не набит: в %s не было ни строки", table)
		}
	}
	if _, err := store.GetProjectByKey(ctx, store.DB(), "SDLC"); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("проект удалённой команды находится: %v", err)
	}
	if _, err := store.GetProjectByKey(ctx, store.DB(), second.Key); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("второй проект удалённой команды находится: %v", err)
	}

	// Соседи целы: не только строка команды, но и содержимое её проекта.
	if _, err := store.GetProjectByKey(ctx, store.DB(), "OTHER"); err != nil {
		t.Fatalf("удаление задело проект соседей: %v", err)
	}
	for table, want := range map[string]int{"repositories": 1, "repo_index_runs": 1, "code_files": 1} {
		if n := countRows(t, ctx, store, fmt.Sprintf(projectScopes[table], neighbour.ID)); n != want {
			t.Fatalf("у соседнего проекта в %s осталось %d вместо %d", table, n, want)
		}
	}
	neighbourAfter := teamContents(t, ctx, store, neighbourTeam.ID)
	for table, want := range neighbourBefore {
		if neighbourAfter[table] != want {
			t.Fatalf("у соседней команды в %s осталось %d вместо %d", table, neighbourAfter[table], want)
		}
	}
	for _, table := range []string{"projects", "team_members", "github_installations"} {
		if neighbourBefore[table] == 0 {
			t.Fatalf("соседняя команда не набита: в %s не было ни строки", table)
		}
	}
	if role, err := store.EffectiveRole(ctx, store.DB(), neighbour.ID, bob.Principal.ID); err != nil ||
		role != domain.MemberAdmin {
		t.Fatalf("лид соседней команды потерял свой проект: %q (%v)", role, err)
	}

	// Участие через ушедшую команду исчезло, поимённое осталось.
	if role, err := store.EffectiveRole(ctx, store.DB(), neighbour.ID, alice.Principal.ID); err != nil ||
		role != domain.MemberViewer {
		t.Fatalf("после удаления команды alice в чужом проекте %q (%v), ожидали поимённый viewer", role, err)
	}

	// Люди остаются людьми: принципалы, их личные команды и инстансовые токены
	// на месте.
	if n := countRows(t, ctx, store, `SELECT count(*) FROM principals`); n != principalsBefore {
		t.Fatalf("принципалов было %d, стало %d", principalsBefore, n)
	}
	if _, err := store.GetTeamByKey(ctx, store.DB(), personal.Key); err != nil {
		t.Fatalf("личная команда участника ушла вместе с организацией: %v", err)
	}
	if n := countRows(t, ctx, store,
		`SELECT count(*) FROM agent_tokens WHERE project_id IS NULL AND token_hash = 'hash-clab-13-instance'`); n != 1 {
		t.Fatalf("инстансовый токен участника отозван вместе с командой: %d", n)
	}

	// Блобы общие по sha256: их чистит сборка мусора, а не удаление арендатора.
	if n := countRows(t, ctx, store, `SELECT count(*) FROM blobs`); n != blobsBefore {
		t.Fatalf("блобов было %d, стало %d", blobsBefore, n)
	}

	// Повторное удаление — уже нечего удалять.
	if _, _, err := auth.DeleteTeam(ctx, admin, owner.Key); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("повторное удаление: %v", err)
	}
	if _, _, err := auth.DeleteTeam(ctx, admin, "нет-такой"); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("удаление несуществующей команды: %v", err)
	}
}

// След об удалении арендатора остаётся только в логе: team_events уходят
// каскадом вместе с командой, а аудита уровня инстанса у системы нет. Значит,
// строка в логе обязана нести всё, по чему потом разбирать случившееся.
func TestDeleteTeamLeavesTraceInLog(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	projectWithEverything(t, ctx, store, engine, system)
	owner := ownerTeamOfSeedProject(t, ctx, store)
	installOn(t, ctx, store, owner.ID, system.ID, 777, "SDLC")

	var buf bytes.Buffer
	auth := NewAuth(store, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})), AuthHeader)
	if _, _, err := auth.DeleteTeam(ctx, Identity{Principal: system}, owner.Key); err != nil {
		t.Fatal(err)
	}

	line := buf.String()
	for _, want := range []string{
		"арендатор удалён", "team=" + owner.Key, "actor=" + system.Handle,
		"projects=1", "SDLC", "installations=1",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("в логе нет %q:\n%s", want, line)
		}
	}
}

// Удалять арендатора вправе его лид, а не любой участник и не посторонний;
// личная команда не удаляется никогда, включая администратора инстанса.
func TestDeleteTeamIsForLeadAndNeverPersonal(t *testing.T) {
	ctx := context.Background()
	store, _, _, auth, _ := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)

	if _, err := auth.CreateTeam(ctx, admin, CreateTeamInput{Key: "platform", Name: "Платформа"}); err != nil {
		t.Fatal(err)
	}
	lead := identityOf(t, store, "lead")
	member := identityOf(t, store, "member")
	stranger := identityOf(t, store, "stranger")
	if _, err := auth.SetTeamMember(ctx, admin, "platform", "user:lead", domain.TeamRoleLead); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetTeamMember(ctx, admin, "platform", "user:member", domain.TeamRoleMember); err != nil {
		t.Fatal(err)
	}

	// Рядовой участник и посторонний уходят ни с чем, и по коду ошибки
	// посторонний не узнаёт, что команда существует и кто в ней состоит.
	if _, _, err := auth.DeleteTeam(ctx, member, "platform"); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("рядовой участник удалил команду: %v", err)
	}
	if _, _, err := auth.DeleteTeam(ctx, stranger, "platform"); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("посторонний удалил команду: %v", err)
	}
	if _, err := store.GetTeamByKey(ctx, store.DB(), "platform"); err != nil {
		t.Fatalf("отказ не оставил команду на месте: %v", err)
	}

	// Личная команда не удаляется ни своим человеком, ни администратором
	// инстанса: она уходит вместе с человеком.
	personal, err := auth.PersonalTeam(ctx, lead)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := auth.DeleteTeam(ctx, lead, personal.Key); apperr.As(err).Code != apperr.CodeConflict {
		t.Fatalf("личная команда удалена своим человеком: %v", err)
	}
	if _, _, err := auth.DeleteTeam(ctx, admin, personal.Key); apperr.As(err).Code != apperr.CodeConflict {
		t.Fatalf("личная команда удалена администратором: %v", err)
	}
	if _, err := store.GetTeamByKey(ctx, store.DB(), personal.Key); err != nil {
		t.Fatalf("личная команда исчезла: %v", err)
	}

	// Лид удаляет свою команду сам, без администратора инстанса.
	if _, _, err := auth.DeleteTeam(ctx, lead, "platform"); err != nil {
		t.Fatalf("лид не смог удалить свою команду: %v", err)
	}
	if n := countRows(t, ctx, store, `SELECT count(*) FROM teams WHERE key = 'platform'`); n != 0 {
		t.Fatalf("после удаления строк команды %d", n)
	}

	// Администратор инстанса проходит тем же путём — через teamRole, без
	// отдельной проверки на RequireAdmin в ручке.
	if _, err := auth.CreateTeam(ctx, admin, CreateTeamInput{Key: "foreign", Name: "Чужая"}); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetTeamMember(ctx, admin, "foreign", "user:lead", domain.TeamRoleLead); err != nil {
		t.Fatal(err)
	}
	if _, _, err := auth.DeleteTeam(ctx, admin, "foreign"); err != nil {
		t.Fatalf("администратор инстанса не смог удалить чужую организацию: %v", err)
	}
}

// Удаление идёт одной транзакцией: ошибка в середине оставляет команду и её
// проекты целиком, полуудалённого арендатора не возникает.
func TestDeleteTeamRollsBackWhole(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	projectWithEverything(t, ctx, store, engine, system)

	owner := ownerTeamOfSeedProject(t, ctx, store)
	installOn(t, ctx, store, owner.ID, system.ID, 777, "SDLC")
	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	teamBefore := teamContents(t, ctx, store, owner.ID)
	projectBefore := projectContents(t, ctx, store, project.ID)

	boom := errors.New("искусственная ошибка после удаления")
	err = store.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := deleteProjectTx(ctx, tx, store, project, system.ID, false); err != nil {
			return err
		}
		if err := store.DeleteTeam(ctx, tx, owner.ID); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("ожидалась искусственная ошибка: %v", err)
	}

	if _, err := store.GetTeamByKey(ctx, store.DB(), owner.Key); err != nil {
		t.Fatalf("команда не вернулась после отката: %v", err)
	}
	for table, want := range teamBefore {
		if n := countRows(t, ctx, store, fmt.Sprintf(teamScopes[table], owner.ID)); n != want {
			t.Fatalf("после отката в %s осталось %d строк вместо %d", table, n, want)
		}
	}
	for table, want := range projectBefore {
		if n := countRows(t, ctx, store, fmt.Sprintf(projectScopes[table], project.ID)); n != want {
			t.Fatalf("после отката в %s проекта осталось %d строк вместо %d", table, n, want)
		}
	}
}

// Два одновременных удаления одного ключа выстраиваются в очередь на строке
// команды: одно проходит, второе видит, что удалять нечего. Ошибки базы на
// внешних ключах при этом не возникает.
func TestDeleteTeamRacesOnLock(t *testing.T) {
	ctx := context.Background()
	store, engine, _, auth, system := setupPhase4(t, AuthHeader)
	admin := Identity{Principal: system}
	projectWithEverything(t, ctx, store, engine, system)
	owner := ownerTeamOfSeedProject(t, ctx, store)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	for i := range errs {
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = auth.DeleteTeam(ctx, admin, owner.Key)
		}(i)
	}
	wg.Wait()

	var ok, gone int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case apperr.As(err).Code == apperr.CodeNotFound:
			gone++
		default:
			t.Fatalf("неожиданная ошибка параллельного удаления: %v", err)
		}
	}
	if ok != 1 || gone != 1 {
		t.Fatalf("параллельное удаление: прошло %d, «не найдено» %d", ok, gone)
	}
}

// Агент, державший аренду этапа в проекте удалённой команды, получает «не
// найдено»: проект ушёл вместе с арендатором, и работать больше не с чем.
// Доставка вебхука по её установке ни к чему не приводит.
func TestAgentAndWebhookOfDeletedTeam(t *testing.T) {
	ctx := context.Background()
	store, engine, _, auth, system := setupPhase4(t, AuthHeader)
	admin := Identity{Principal: system}
	owner := ownerTeamOfSeedProject(t, ctx, store)
	installOn(t, ctx, store, owner.ID, system.ID, 777, "SDLC")

	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Живая задача"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(ctx, task.Key, "start", system.ID, "поехали"); err != nil {
		t.Fatal(err)
	}
	claimed, err := engine.Claim(ctx, task.Key, system.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	// До удаления доставка по установке ставит прогон индексации: иначе
	// проверка «после удаления ничего не происходит» ничего не значит.
	push := []byte(`{"ref":"refs/heads/main","after":"abc123abc123abc123abc123abc123abc123abc1",
		"repository":{"name":"sdlc-pipeline-backend","owner":{"login":"cadence-lab-tech"}},
		"installation":{"id":777}}`)
	if out, err := engine.HandleGitHubEvent(ctx, "push", "d-before", push); err != nil || out.Queued != 1 {
		t.Fatalf("до удаления push не встал в очередь: %v %+v", err, out)
	}

	if _, _, err := auth.DeleteTeam(ctx, admin, owner.Key); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.Heartbeat(ctx, claimed.StageRunID, system.ID); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("heartbeat после удаления команды: код %q, ошибка %v", apperr.As(err).Code, err)
	}
	if _, err := engine.SubmitArtifact(ctx, task.Key, system.ID, SubmitArtifactInput{
		Kind: "analysis.brief", Title: "Бриф", Content: []byte("# Бриф"),
	}); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("сдача артефакта после удаления команды: код %q, ошибка %v", apperr.As(err).Code, err)
	}
	if _, err := engine.Execute(ctx, task.Key, "analysis_done", system.ID, ""); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("переход после удаления команды: код %q, ошибка %v", apperr.As(err).Code, err)
	}

	queuedBefore := countRows(t, ctx, store, `SELECT count(*) FROM repo_index_runs`)
	out, err := engine.HandleGitHubEvent(ctx, "push", "d-after", push)
	if err != nil {
		t.Fatalf("доставка по ушедшей установке обвалилась: %v", err)
	}
	if out.Queued != 0 || out.Ignored == "" {
		t.Fatalf("доставка по ушедшей установке что-то сделала: %+v", out)
	}
	if n := countRows(t, ctx, store, `SELECT count(*) FROM repo_index_runs`); n != queuedBefore {
		t.Fatalf("прогонов индексации было %d, стало %d", queuedBefore, n)
	}
}
