package main

// Проект из git remote: clab в каталоге репозитория, привязанного к проекту,
// не требует --project. Как gh узнаёт репозиторий по origin.

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// parseRemote достаёт owner/name из адреса remote в любом из ходовых видов:
// git@github.com:acme/api.git, https://github.com/acme/api,
// ssh://git@github.com/acme/api.git.
func parseRemote(raw string) (owner, name string, ok bool) {
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

// gitRemote — origin текущего каталога; пусто, если это не репозиторий.
func gitRemote(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "git", "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// projectByRemote спрашивает у API, к какому проекту привязан репозиторий
// из origin. Ровно один — он и есть проект; несколько — пусть человек назовёт.
func (c *Client) projectByRemote(ctx context.Context) (string, error) {
	owner, name, ok := parseRemote(gitRemote(ctx))
	if !ok {
		return "", nil
	}
	var out struct {
		Projects []struct {
			Key string `json:"key"`
		} `json:"projects"`
	}
	if err := c.get(ctx, "/projects", query("repo", owner+"/"+name), &out); err != nil {
		return "", err
	}
	switch len(out.Projects) {
	case 0:
		return "", nil
	case 1:
		return out.Projects[0].Key, nil
	default:
		keys := make([]string, 0, len(out.Projects))
		for _, p := range out.Projects {
			keys = append(keys, p.Key)
		}
		return "", fmt.Errorf("репозиторий %s/%s привязан к нескольким проектам (%s): укажите --project",
			owner, name, strings.Join(keys, ", "))
	}
}
