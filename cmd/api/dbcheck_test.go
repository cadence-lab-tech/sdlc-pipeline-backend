package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dbcheck берёт адрес тем же разбором, что сервис (CLAB-200): файл, который
// не читается, и обе формы — пункт url отчёта с кодом 1, без значения.
func TestDBCheckSecretFiles(t *testing.T) {
	const secret = "pw-6b1e"
	dir := t.TempDir()
	urlFile := filepath.Join(dir, "db-url")
	if err := os.WriteFile(urlFile, []byte("postgres://u:"+secret+"@db/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		env  map[string]string
		want string
	}{
		"missing file": {map[string]string{"SDLC_DATABASE_URL": "", "SDLC_DATABASE_URL_FILE": filepath.Join(dir, "nope")},
			"SDLC_DATABASE_URL_FILE"},
		"both forms": {map[string]string{"SDLC_DATABASE_URL": "postgres://u:" + secret + "@db/x", "SDLC_DATABASE_URL_FILE": urlFile},
			"both SDLC_DATABASE_URL and SDLC_DATABASE_URL_FILE"},
		"password twice": {map[string]string{"SDLC_DATABASE_URL": "", "SDLC_DATABASE_URL_FILE": urlFile, "SDLC_DATABASE_PASSWORD_FILE": urlFile},
			"SDLC_DATABASE_PASSWORD_FILE"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			var out, errw bytes.Buffer
			if code := dbcheck(nil, &out, &errw); code != 1 || !strings.Contains(out.String(), c.want) ||
				strings.Contains(out.String()+errw.String(), secret) {
				t.Fatalf("code %d: %s %s", code, out.String(), errw.String())
			}
			out.Reset()
			if code := dbcheck([]string{"--json"}, &out, &errw); code != 1 {
				t.Fatalf("--json code %d", code)
			}
			var report struct {
				OK     bool `json:"ok"`
				Checks []struct {
					Name, Status, Detail string
				} `json:"checks"`
			}
			if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.OK || len(report.Checks) != 1 ||
				report.Checks[0].Name != "url" || !strings.Contains(report.Checks[0].Detail, c.want) {
				t.Fatalf("--json: %v %s", err, out.String())
			}
		})
	}
}
