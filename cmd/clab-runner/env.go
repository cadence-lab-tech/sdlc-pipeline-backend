package main

import (
	"sort"
	"strings"
)

// Окружение агента — по списку разрешённых (решение 0049, CLAB-213).
//
// Раньше агент наследовал окружение раннера целиком: токен раннера,
// SDLC_RUNNER_* и любые секреты оболочки сотрудника. Теперь он получает
// только то, без чего не работают оболочка, git по SSH и агент команды по
// умолчанию, плюс названное администратором в SDLC_RUNNER_ENV, плюс то, что
// раннер задаёт сам.

// baseAgentEnv — базовый список: имена и префиксы со звёздочкой на конце.
var baseAgentEnv = []string{
	// оболочка и пользователь
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TERM", "TZ",
	"LANG", "LC_*", "XDG_*",
	// сеть за прокси и свои корневые сертификаты
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
	// git по SSH
	"SSH_AUTH_SOCK",
	// агент команды по умолчанию — Claude Code
	"ANTHROPIC_*", "CLAUDE_*",
}

// reservedEnv — префиксы, которые из окружения раннера не проходят никогда,
// даже если названы в SDLC_RUNNER_ENV: в них токен раннера, SDLC_ACTOR и
// настройки самого раннера. Нужные агенту SDLC_* и CLAB_CONFIG_DIR раннер
// задаёт сам.
var reservedEnv = []string{"SDLC_", "CLAB_"}

func reserved(name string) bool {
	upper := strings.ToUpper(name)
	for _, p := range reservedEnv {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	return false
}

// parseAgentEnv разбирает SDLC_RUNNER_ENV: имена и префиксы через запятую
// (GH_TOKEN,AWS_*). Имена SDLC_* и CLAB_* возвращаются отдельно — раннер
// говорит о них при старте, а передавать не будет.
func parseAgentEnv(raw string) (allow, refused []string) {
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		switch {
		case name == "":
		case reserved(name):
			refused = append(refused, name)
		default:
			allow = append(allow, name)
		}
	}
	return allow, refused
}

// matchEnv — подходит ли имя под шаблон: точное имя или префикс с * на конце.
func matchEnv(pattern, name string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(name, prefix)
	}
	return pattern == name
}

// agentEnv собирает окружение агента: из parent проходят базовый список и
// extra, кроме SDLC_* и CLAB_*; затем set перекрывает всё. Чистая функция:
// окружение раннера передаётся явно, результат отсортирован.
func agentEnv(parent, extra []string, set map[string]string) []string {
	allow := append(append([]string{}, baseAgentEnv...), extra...)
	out := map[string]string{}
	for _, kv := range parent {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" || reserved(name) {
			continue
		}
		for _, p := range allow {
			if matchEnv(p, name) {
				out[name] = value
				break
			}
		}
	}
	for name, value := range set {
		if value != "" {
			out[name] = value
		}
	}

	env := make([]string, 0, len(out))
	for name, value := range out {
		env = append(env, name+"="+value)
	}
	sort.Strings(env)
	return env
}
