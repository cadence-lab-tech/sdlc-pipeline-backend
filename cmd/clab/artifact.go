package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// cmdArtifact печатает содержимое артефакта (CLAB-222). Исполнитель этапа
// работает без MCP, только `clab` с токеном задачи, и входные артефакты
// читает этой командой: id — из prior_artifacts ответа `clab claim` и
// `clab show`. Видимость решает сервер: токену задачи открыты артефакты
// своей задачи, её предков, детей и связанных задач (решение 0049).
//
// Без --json — содержимое как есть, чтобы его можно было перенаправить в
// файл; с --json — {id, media_type, content}.
func cmdArtifact(ctx context.Context, c *Client, args []string) error {
	id, rest := popTask(args)
	if _, err := flags("artifact", rest, func(*flag.FlagSet) {}); err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("clab artifact <id> — id из prior_artifacts ответа clab claim или clab show")
	}
	raw, mediaType, err := c.getContent(ctx, "/artifacts/"+url.PathEscape(id)+"/content")
	if err != nil {
		return err
	}
	if jsonOut {
		return print(map[string]any{"id": id, "media_type": mediaType, "content": string(raw)})
	}
	if _, err := os.Stdout.Write(raw); err != nil {
		return err
	}
	if len(raw) > 0 && !strings.HasSuffix(string(raw), "\n") {
		fmt.Println()
	}
	return nil
}
