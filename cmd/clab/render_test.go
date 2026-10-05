package main

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Подписи источника совпадают с колонкой «Чем проверяли» в настройках
// проекта; незнакомое значение от сервера новее CLI показывается как есть.
func TestCredentialSourceLabels(t *testing.T) {
	installation := int64(42)
	cases := []struct {
		source       string
		installation *int64
		ref          string
		want         string
	}{
		{"repository_secret", nil, "repo-x", "секрет репозитория repo-x"},
		{"repository_installation", &installation, "", "установка репозитория 42"},
		{"project_installation", &installation, "", "установка проекта 42"},
		{"project_secret", nil, "proj", "секрет проекта proj"},
		{"instance", nil, "", "токен инстанса"},
		{"", nil, "", "—"},
		{"something_new", nil, "", "something_new"},
	}
	for _, tc := range cases {
		if got := credentialSource(tc.source, tc.installation, tc.ref); got != tc.want {
			t.Fatalf("%q: %q вместо %q", tc.source, got, tc.want)
		}
	}
}

// ── состояние приложения GitHub и нечитаемые репозитории (CLAB-79) ──────────

const (
	checkRejected = `{"app_configured":true,"app":{"state":"rejected","error":"github 401 на утверждение приложения: {\"message\":\"Bad credentials\"}","checked_at":"2026-10-05T14:00:00Z","clock_offset_seconds":180},"repositories":[{"repository":"acme/api","ok":false,"error":"приложение GitHub инстанса не принято провайдером","source":"repository_installation","installation_id":42}]}`
	checkOK       = `{"app_configured":true,"app":{"state":"ok","checked_at":"2026-10-05T14:00:00Z"},"repositories":[]}`
	checkUnknown  = `{"app_configured":true,"app":{"state":"unknown","error":"github 500 на утверждение приложения: upstream","checked_at":"2026-10-05T14:00:00Z"},"repositories":[]}`
	checkNoApp    = `{"app_configured":false,"app":{"state":"not_configured"},"repositories":[]}`
	// Ответы сервера до CLAB-79: поля app нет.
	checkLegacy      = `{"app_configured":true,"repositories":[{"repository":"acme/api","ok":true,"source":"instance"}]}`
	checkLegacyNoApp = `{"app_configured":false,"repositories":[]}`
)

func runGitHubCheck(t *testing.T, reply string) string {
	t.Helper()
	c, rec := newInviteClient(t, 200, reply)
	out, _, err := capture(t, func() error {
		return cmdProject(context.Background(), c, []string{"github", "--check", "--project", "CLAB"})
	})
	if err != nil {
		t.Fatalf("проверка: %v", err)
	}
	if rec.method != "GET" || rec.path != "/v1/projects/CLAB/github/check" {
		t.Fatalf("запрос %s %s", rec.method, rec.path)
	}
	return out
}

// C1: отказ ключа назван до таблицы — с текстом провайдера, тем, что
// проверить, и расхождением часов.
func TestGitHubCheckNamesRejectedKey(t *testing.T) {
	withJSON(t, false)
	out := runGitHubCheck(t, checkRejected)
	for _, want := range []string{
		"ключ не принят", "Bad credentials", "администратор инстанса",
		"SDLC_GITHUB_APP_KEY_FILE", "SDLC_GITHUB_APP_ID", "часы машины",
		"отстают от времени провайдера на 3m0s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "ключ не принят") > strings.Index(out, "РЕПОЗИТОРИЙ") {
		t.Fatalf("состояние приложения напечатано после таблицы:\n%s", out)
	}
}

// C2: остальные три состояния — три разные строки, и «не удалось
// проверить» не называет ключ непринятым.
func TestGitHubCheckStates(t *testing.T) {
	withJSON(t, false)
	ok := runGitHubCheck(t, checkOK)
	unknown := runGitHubCheck(t, checkUnknown)
	noApp := runGitHubCheck(t, checkNoApp)

	if !strings.Contains(ok, "ключ принят") {
		t.Errorf("ok:\n%s", ok)
	}
	if !strings.Contains(unknown, "не удалось проверить") || !strings.Contains(unknown, "github 500") {
		t.Errorf("unknown:\n%s", unknown)
	}
	if strings.Contains(unknown, "не принят") {
		t.Errorf("unknown называет ключ непринятым:\n%s", unknown)
	}
	if !strings.Contains(noApp, "Приложение у инстанса не настроено") {
		t.Errorf("not_configured:\n%s", noApp)
	}
	first := func(s string) string { return strings.SplitN(s, "\n", 2)[0] }
	if first(ok) == first(unknown) || first(ok) == first(noApp) || first(unknown) == first(noApp) {
		t.Fatalf("строки состояний совпадают: %q, %q, %q", first(ok), first(unknown), first(noApp))
	}
}

// Состояние от сервера новее CLI показывается как есть, а часы, которые
// спешат, названы спешащими.
func TestAppStatusLinesEdges(t *testing.T) {
	lines := appStatusLines(&appStatusView{State: "throttled"}, true)
	if len(lines) != 1 || !strings.Contains(lines[0], "throttled") {
		t.Fatalf("незнакомое состояние: %q", lines)
	}
	ahead := -90
	lines = appStatusLines(&appStatusView{State: "ok", ClockOffsetSeconds: &ahead}, true)
	if len(lines) != 2 || !strings.Contains(lines[1], "спешат") || !strings.Contains(lines[1], "1m30s") {
		t.Fatalf("спешащие часы: %q", lines)
	}
}

// C6: сервер без поля app — вывод как до правки: одна строка про
// app_configured либо ничего, без пустых строк состояния.
func TestGitHubCheckAgainstOldServer(t *testing.T) {
	withJSON(t, false)
	out := runGitHubCheck(t, checkLegacy)
	if !strings.HasPrefix(out, "РЕПОЗИТОРИЙ") {
		t.Fatalf("перед таблицей появилось лишнее:\n%s", out)
	}
	out = runGitHubCheck(t, checkLegacyNoApp)
	if !strings.HasPrefix(out, "Приложение у инстанса не настроено: установки не выписывают токенов.\n") {
		t.Fatalf("строка про ненастроенное приложение изменилась:\n%s", out)
	}
}

const (
	teamInstallations       = `{"app":{"state":"rejected","error":"github 401 на утверждение приложения: {\"message\":\"Bad credentials\"}","checked_at":"2026-10-05T14:00:00Z"},"installations":[{"installation_id":42,"account_login":"acme","account_type":"Organization","created_at":"2026-09-11T10:00:00Z"},{"installation_id":43,"account_login":"old","account_type":"User","created_at":"2026-09-11T10:00:00Z","removed_at":"2026-09-20T10:00:00Z"}]}`
	teamInstallationsOK     = `{"app":{"state":"ok","checked_at":"2026-10-05T14:00:00Z"},"installations":[{"installation_id":42,"account_login":"acme","account_type":"Organization","created_at":"2026-09-11T10:00:00Z"}]}`
	teamInstallationsLegacy = `{"installations":[{"installation_id":42,"account_login":"acme","account_type":"Organization","created_at":"2026-09-11T10:00:00Z"}]}`
)

// C3: clab team github печатает состояние приложения и установки; живая
// установка при непринятом ключе не названа работающей.
func TestTeamGitHubPrintsAppStateAndInstallations(t *testing.T) {
	withJSON(t, false)
	run := func(reply string) string {
		t.Helper()
		c, rec := newInviteClient(t, 200, reply)
		out, _, err := capture(t, func() error {
			return cmdTeam(context.Background(), c, []string{"github", "acme"})
		})
		if err != nil {
			t.Fatal(err)
		}
		if rec.path != "/v1/teams/acme/github/installations" {
			t.Fatalf("запрос %s", rec.path)
		}
		return out
	}

	out := run(teamInstallations)
	for _, want := range []string{"ключ не принят", "Bad credentials", "42", "acme", "ключ приложения не принят", "снята"} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "работает") {
		t.Fatalf("установка названа работающей при непринятом ключе:\n%s", out)
	}

	out = run(teamInstallationsOK)
	if !strings.Contains(out, "ключ принят") || !strings.Contains(out, "работает") {
		t.Fatalf("исправное приложение:\n%s", out)
	}

	// C6: сервер без поля app — только установки.
	out = run(teamInstallationsLegacy)
	if !strings.HasPrefix(out, "УСТАНОВКА") || !strings.Contains(out, "работает") {
		t.Fatalf("старый сервер:\n%s", out)
	}
}

const longIndexError = `приложение GitHub инстанса не принято провайдером: github 401 на токен установки 160909220: {"message":"Bad credentials","documentation_url":"https://docs.github.com/rest"}`

func indexReply(failure string) string {
	return `{"repositories":[` +
		`{"repository":"acme/api","index_enabled":true,"branch":"main","commit":"0123456789abcdef","state":"failed","files_indexed":0,"files_reused":0,"chunks_indexed":0,"error":` +
		strconv.Quote(longIndexError) + `,"last_indexed_at":"2026-09-15T10:00:00Z"` + failure + `},` +
		`{"repository":"acme/web","index_enabled":true,"branch":"main","commit":"fedcba9876543210","state":"completed","files_indexed":3,"files_reused":40,"chunks_indexed":120,"last_indexed_at":"2026-10-05T10:00:00Z"}]}`
}

func runIndex(t *testing.T, reply string) string {
	t.Helper()
	c, _ := newInviteClient(t, 200, reply)
	out, _, err := capture(t, func() error {
		return cmdIndex(context.Background(), c, []string{"--project", "CLAB"})
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// C4: под таблицей по нечитаемому репозиторию — полный текст ошибки и с
// какого времени; в колонке ошибка по-прежнему обрезана.
func TestIndexPrintsFullFailure(t *testing.T) {
	withJSON(t, false)
	since := time.Date(2026, 9, 15, 10, 30, 0, 0, time.UTC)
	out := runIndex(t, indexReply(`,"failure":{"since":"2026-09-15T10:30:00Z","error":`+
		strconv.Quote(longIndexError)+`,"source":"run"}`))

	table, tail, found := strings.Cut(out, "\n\n")
	if !found {
		t.Fatalf("под таблицей ничего нет:\n%s", out)
	}
	if strings.Contains(table, longIndexError) {
		t.Fatalf("колонка прогона перестала резать ошибку:\n%s", table)
	}
	for _, want := range []string{"acme/api не читается с " + when(&since), longIndexError} {
		if !strings.Contains(tail, want) {
			t.Errorf("под таблицей нет %q:\n%s", want, tail)
		}
	}
	if strings.Contains(tail, "acme/web") {
		t.Fatalf("читаемый репозиторий попал в нечитаемые:\n%s", tail)
	}
}

// C6: сервер без поля failure — таблица как раньше, под ней ничего.
func TestIndexAgainstOldServer(t *testing.T) {
	withJSON(t, false)
	out := runIndex(t, indexReply(""))
	if strings.Contains(out, "не читается") || strings.Contains(out, "\n\n") {
		t.Fatalf("вывод изменился без поля failure:\n%s", out)
	}
	if !strings.Contains(out, "acme/api") || !strings.Contains(out, "acme/web") {
		t.Fatalf("таблица потеряла строки:\n%s", out)
	}
}

// C5: --json отдаёт ответ сервера как есть, с полями app и failure.
func TestGitHubAndIndexJSONPassThrough(t *testing.T) {
	withJSON(t, true)
	if out := runGitHubCheck(t, checkRejected); !strings.Contains(out, `"app"`) ||
		!strings.Contains(out, `"clock_offset_seconds": 180`) || !strings.Contains(out, `"state": "rejected"`) {
		t.Fatalf("--json потерял app:\n%s", out)
	}
	out := runIndex(t, indexReply(`,"failure":{"since":"2026-09-15T10:30:00Z","error":"x","source":"head"}`))
	if !strings.Contains(out, `"failure"`) || !strings.Contains(out, `"source": "head"`) {
		t.Fatalf("--json потерял failure:\n%s", out)
	}
	c, _ := newInviteClient(t, 200, teamInstallations)
	out, _, err := capture(t, func() error {
		return cmdTeam(context.Background(), c, []string{"github", "acme"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"app"`) || !strings.Contains(out, `"installations"`) {
		t.Fatalf("--json потерял поля:\n%s", out)
	}
}
