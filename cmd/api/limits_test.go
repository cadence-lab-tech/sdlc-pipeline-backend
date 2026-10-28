package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
)

// AC-5.7: журнал старта называет сроки и предел тела — по нему
// администратор разбирает оборванную загрузку.
func TestLogRequestLimits(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	logRequestLimits(log, config.Config{
		RequestTimeout: 30 * time.Second, UploadTimeout: 15 * time.Minute,
		DownloadTimeout: 0, BlobMaxBytes: 64 << 20,
	})
	line := buf.String()
	for _, want := range []string{"request_timeout=30s", "upload_timeout=15m0s", "download_timeout=0s",
		"max_body_bytes=135266304"} {
		if !strings.Contains(line, want) {
			t.Errorf("в журнале старта нет %s: %s", want, line)
		}
	}
}
