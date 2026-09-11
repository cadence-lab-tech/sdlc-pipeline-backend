package main

// Проект из git remote: clab в каталоге репозитория, привязанного к проекту,
// не требует --project. Как gh узнаёт репозиторий по origin.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/gitremote"
)

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
	owner, name, ok := gitremote.Parse(gitRemote(ctx))
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
