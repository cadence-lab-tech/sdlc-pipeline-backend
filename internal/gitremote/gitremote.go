// Package gitremote разбирает адрес git remote в owner/name.
//
// Разбор нужен двум командам: clab узнаёт проект по origin текущего
// каталога, clab-runner находит клон репозитория из области задачи по его
// origin. Код один на двоих, иначе они разойдутся в том, какой адрес каким
// репозиторием считать.
package gitremote

import (
	"regexp"
	"strings"
)

// Parse достаёт owner/name из адреса remote в любом из ходовых видов:
// git@github.com:acme/api.git, https://github.com/acme/api,
// ssh://git@github.com/acme/api.git.
func Parse(raw string) (owner, name string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	// scp-подобная форма: git@host:owner/name.git
	if m := scpRemote.FindStringSubmatch(raw); m != nil {
		return m[1], strings.TrimSuffix(m[2], ".git"), true
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	}
	if i := strings.Index(raw, "/"); i >= 0 {
		raw = raw[i+1:]
	} else {
		return "", "", false
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimRight(raw, "/"), ".git"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

var scpRemote = regexp.MustCompile(`^[\w.-]+@[\w.-]+:([\w.-]+)/([\w.-]+)$`)
