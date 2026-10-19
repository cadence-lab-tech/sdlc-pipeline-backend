package postgres

import (
	"strings"
	"testing"
)

func TestCheckURL(t *testing.T) {
	cases := []struct {
		url, target, sslmode, err string
	}{
		{url: "postgres://cadence:s3cret@db.internal:5432/cadence?sslmode=verify-full&sslrootcert=/secrets/db-ca.pem",
			target: "db.internal:5432/cadence", sslmode: "verify-full"},
		{url: "postgresql://cadence:pw@db.internal/cadence?sslmode=require", target: "db.internal:5432/cadence", sslmode: "require"},
		{url: "postgres://cadence:pw@[::1]/cadence?sslmode=disable", target: "[::1]:5432/cadence", sslmode: "disable"},
		{url: "postgres://cadence:pw@db.internal:5432/cadence?sslmode=require&pool_max_conns=5", err: "pool_max_conns"},
		{url: "postgres://cadence:pw@db.internal:5432/cadence?search_path=x", err: "search_path"},
		{url: "host=db user=cadence", err: "must start with postgres://"},
		{url: "postgres://cadence:pw@db.internal:5432/?sslmode=require", err: "names no database"},
		{url: "", err: "empty"},
	}
	for _, c := range cases {
		p, err := checkURL(c.url)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("checkURL(%q): want error with %q, got %v", c.url, c.err, err)
			}
			if err != nil && strings.Contains(err.Error(), "pw") {
				t.Errorf("checkURL(%q): the error repeats the password: %v", c.url, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("checkURL(%q): %v", c.url, err)
			continue
		}
		if p.target != c.target || p.sslmode != c.sslmode {
			t.Errorf("checkURL(%q) = %q %q, want %q %q", c.url, p.target, p.sslmode, c.target, c.sslmode)
		}
	}
}

func TestTLSPolicy(t *testing.T) {
	cases := map[string]string{
		"":                              CheckFail,
		"sslmode=verify-full":           CheckFail, // без sslrootcert
		"sslmode=verify-full&ca=/x.pem": CheckOK,
		"sslmode=verify-ca&ca=system":   CheckOK,
		"sslmode=require":               CheckWarn,
		"sslmode=prefer":                CheckWarn,
		"sslmode=allow":                 CheckWarn,
		"sslmode=disable":               CheckWarn,
		"sslmode=bogus":                 CheckFail,
	}
	for in, want := range cases {
		var p urlParams
		for _, kv := range strings.Split(in, "&") {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "sslmode":
				p.sslmode = v
			case "ca":
				p.sslrootcert = v
			}
		}
		if got, detail, _ := tlsPolicy(p); got != want {
			t.Errorf("tlsPolicy(%q) = %s (%s), want %s", in, got, detail, want)
		}
	}
	if _, detail, _ := tlsPolicy(urlParams{sslmode: "disable"}); !strings.Contains(detail, "UNENCRYPTED") {
		t.Errorf("sslmode=disable must warn loudly, got %q", detail)
	}
}

func TestVersionBelow(t *testing.T) {
	for _, c := range []struct {
		have, min string
		below     bool
	}{
		{"0.4.4", "0.5.0", true}, {"0.5.0", "0.5.0", false}, {"0.8.0", "0.5.0", false},
		{"0.10.1", "0.5.0", false}, {"1.0", "0.5.0", false},
	} {
		if got := versionBelow(c.have, c.min); got != c.below {
			t.Errorf("versionBelow(%s, %s) = %v", c.have, c.min, got)
		}
	}
}

func TestScrub(t *testing.T) {
	if got := scrub("password authentication failed for p@ss/w0rd and p%40ss%2Fw0rd", "p@ss/w0rd"); strings.Contains(got, "w0rd") {
		t.Errorf("scrub left the password: %s", got)
	}
}

func TestLatestMigration(t *testing.T) {
	v, err := LatestMigration()
	if err != nil || v < 25 {
		t.Fatalf("LatestMigration() = %d, %v", v, err)
	}
}
