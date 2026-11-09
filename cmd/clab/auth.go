package main

// Вход как у gh (решение 0032): clab заводит запрос входа, показывает код и
// открывает браузер на странице подтверждения; человек, уже вошедший в
// интерфейс, нажимает «Разрешить»; clab опрашивает запрос и получает
// долгоживущий токен, который кладёт в конфиг. Через ssh то же самое: код
// набирается в браузере на другой машине.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func cmdAuth(ctx context.Context, c *Client, args []string) error {
	sub, rest := popTask(args)
	switch sub {
	case "login":
		return authLogin(ctx, c, rest)
	case "status", "":
		return authStatus(ctx, c, rest)
	case "logout":
		return authLogout(ctx, c, rest)
	case "token":
		if _, err := flags("auth", rest, func(*flag.FlagSet) {}); err != nil {
			return err
		}
		token, err := authToken(c)
		if err != nil {
			return err
		}
		fmt.Println(token)
		return nil
	default:
		return fmt.Errorf("clab auth login|status|logout|token")
	}
}

type loginStart struct {
	UserCode        string    `json:"user_code"`
	PollSecret      string    `json:"poll_secret"`
	ExpiresAt       time.Time `json:"expires_at"`
	VerifyPath      string    `json:"verify_path"`
	IntervalSeconds int       `json:"interval_seconds"`
}

type loginPoll struct {
	Status string `json:"status"`
	Token  string `json:"token"`
	Record *struct {
		ID        string     `json:"id"`
		Name      string     `json:"name"`
		ExpiresAt *time.Time `json:"expires_at"`
	} `json:"record"`
	Principal *struct {
		Kind   string `json:"kind"`
		Handle string `json:"handle"`
	} `json:"principal"`
}

func authLogin(ctx context.Context, c *Client, args []string) error {
	var label string
	var days int
	var noBrowser, withToken bool
	fs, err := flags("auth", args, func(fs *flag.FlagSet) {
		fs.StringVar(&label, "label", "", "подпись токена; по умолчанию «clab на <машина>»")
		fs.IntVar(&days, "days", 365, "срок токена в днях; 0 — бессрочно")
		fs.BoolVar(&noBrowser, "no-browser", false, "не открывать браузер, только показать адрес")
		fs.BoolVar(&withToken, "with-token", false, "взять готовый токен из stdin вместо входа через браузер")
	})
	if err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("адрес задаётся флагом: clab auth login --host %s", fs.Arg(0))
	}

	// Без каталога конфига токену некуда лечь: отказ до любого запроса, а
	// не вход, который потом не сохранится.
	if _, err := configDir(); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ch, err := chooseHost(cfg, c.globals, c.marker, true)
	if err != nil {
		return err
	}
	host, origin := ch.host, ch.origin
	if ch.source == hostFromMarker {
		// Хост назван файлом репозитория, а не человеком: адрес и файл
		// печатаются до первого запроса, а готовый токен на такой адрес не
		// отправляется — device flow секретов хосту не шлёт.
		if withToken {
			return fmt.Errorf("хост %s взят из маркера %s; для --with-token укажите --host явно", origin, ch.marker.Path)
		}
		fmt.Fprintf(os.Stderr, "Хост взят из маркера %s: %s\n", ch.marker.Path, origin)
	} else if ch.marker != nil && ch.source == hostFromDefault {
		fmt.Fprintf(os.Stderr, "В маркере %s хоста нет, вход на хост по умолчанию %s\n", ch.marker.Path, host)
	}
	if host == "" {
		return fmt.Errorf("куда входить? clab auth login --host https://sdlc.example.com")
	}
	// Вход идёт на названный хост, а не на тот, что в конфиге по умолчанию.
	c.base = origin + "/v1"
	c.origin = origin
	c.token = ""

	if label == "" {
		name, _ := os.Hostname()
		if name == "" {
			name = "машине"
		}
		label = "clab на " + strings.TrimSuffix(name, ".local")
	}

	var token, tokenID string
	if withToken {
		token, err = readSecret()
		if err != nil {
			return err
		}
	} else {
		token, tokenID, err = loginViaBrowser(ctx, c, origin, label, days, noBrowser)
		if err != nil {
			return err
		}
	}

	// Проверить токен и узнать, кем вошли.
	c.token = token
	var me struct {
		Principal struct {
			Kind   string `json:"kind"`
			Handle string `json:"handle"`
		} `json:"principal"`
		Token *struct {
			ID        string     `json:"id"`
			ExpiresAt *time.Time `json:"expires_at"`
		} `json:"token"`
	}
	if err := c.get(ctx, "/auth/me", nil, &me); err != nil {
		return fmt.Errorf("токен не принят: %w", err)
	}
	if tokenID == "" && me.Token != nil {
		tokenID = me.Token.ID
	}
	handle := me.Principal.Kind + ":" + me.Principal.Handle

	cfg.Hosts[host] = HostConfig{URL: origin, Token: token, TokenID: tokenID, Handle: handle}
	// Вход ради хоста каталога не меняет хост остальных каталогов: по
	// маркеру default_host только заполняется, если его не было.
	keptDefault := ch.source == hostFromMarker && cfg.DefaultHost != "" && cfg.DefaultHost != host
	if !keptDefault {
		cfg.DefaultHost = host
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}

	if jsonOut {
		out := map[string]any{"host": host, "url": origin, "handle": handle, "token_id": tokenID, "config": configPathLabel()}
		if ch.source == hostFromMarker {
			out["host_source"] = hostFromMarker
			out["marker_path"] = ch.marker.Path
			out["default_host"] = cfg.DefaultHost
		}
		return print(out)
	}
	until := "бессрочно"
	if me.Token != nil && me.Token.ExpiresAt != nil {
		until = "до " + when(me.Token.ExpiresAt)
	}
	fmt.Printf("Вошли на %s как %s, токен %s.\n", host, handle, until)
	if keptDefault {
		fmt.Printf("Хост по умолчанию не менялся: %s\n", cfg.DefaultHost)
	}
	fmt.Printf("Конфиг: %s\n", configPathLabel())
	return nil
}

// loginViaBrowser — device flow: код человеку, секрет себе, опрос до ответа.
func loginViaBrowser(ctx context.Context, c *Client, origin, label string, days int, noBrowser bool) (token, tokenID string, err error) {
	var start loginStart
	if err := c.post(ctx, "/auth/cli", map[string]any{"label": label, "token_days": days}, &start); err != nil {
		return "", "", fmt.Errorf("%s не принимает вход CLI: %w", origin, err)
	}
	verify := origin + start.VerifyPath + "?" + url.Values{"code": {start.UserCode}}.Encode()

	fmt.Fprintf(os.Stderr, "Код подтверждения: %s\n", start.UserCode)
	if noBrowser || !openBrowser(verify) {
		fmt.Fprintf(os.Stderr, "Откройте в браузере: %s\n", verify)
	} else {
		fmt.Fprintf(os.Stderr, "Открыт браузер: %s\n", verify)
	}
	fmt.Fprintln(os.Stderr, "Ждём подтверждения…")

	interval := time.Duration(start.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", "", fmt.Errorf("вход прерван")
		case <-ticker.C:
		}
		if time.Now().After(start.ExpiresAt) {
			return "", "", fmt.Errorf("код устарел, никто не подтвердил вход за отведённое время")
		}
		var poll loginPoll
		if err := c.post(ctx, "/auth/cli/poll", map[string]any{"poll_secret": start.PollSecret}, &poll); err != nil {
			return "", "", err
		}
		switch poll.Status {
		case "pending":
			continue
		case "approved":
			if poll.Record != nil {
				tokenID = poll.Record.ID
			}
			return poll.Token, tokenID, nil
		case "denied":
			return "", "", fmt.Errorf("вход отклонён в браузере")
		default:
			return "", "", fmt.Errorf("запрос входа закрыт: %s", poll.Status)
		}
	}
}

// openBrowser открывает адрес системным способом; неудача — не ошибка,
// адрес всё равно напечатан.
func openBrowser(target string) bool {
	if os.Getenv("CLAB_NO_BROWSER") != "" {
		return false
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start() == nil
}

// readSecret читает токен из stdin: строкой из файла или пайпа, как
// «gh auth login --with-token».
func readSecret() (string, error) {
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("токен ожидался в stdin: echo $TOKEN | clab auth login --with-token")
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("в stdin пусто")
	}
	return line, nil
}

// authHostStatus — строка списка входов clab auth status.
type authHostStatus struct {
	Host    string `json:"host"`
	URL     string `json:"url"`
	Handle  string `json:"handle,omitempty"`
	Default bool   `json:"default"`
	Valid   bool   `json:"valid"`
	Error   string `json:"error,omitempty"`
}

// dirStatus — блок «Каталог» clab auth status: проект и хост из маркера
// каталога и вошёл ли на этот хост. Ключи JSON есть всегда: ИИ читает их
// без догадок, отсутствие и пустота — одно.
type dirStatus struct {
	MarkerPath   string `json:"marker_path"`
	Project      string `json:"project"`
	Host         string `json:"host"`
	LoggedIn     bool   `json:"logged_in"`
	Valid        bool   `json:"valid"`
	Handle       string `json:"handle"`
	Error        string `json:"error"`
	Hint         string `json:"hint"`
	OverriddenBy string `json:"overridden_by"`

	hostKey     string // ключ хоста маркера в конфиге
	defaultHost string // хост по умолчанию, если в маркере хоста нет
	override    string // чем перекрыт хост маркера
	broken      bool   // маркер не принят: в Error его ошибка
}

func authStatus(ctx context.Context, c *Client, args []string) error {
	if _, err := flags("auth", args, func(*flag.FlagSet) {}); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	out := []authHostStatus{}
	probeErr := map[string]error{}
	for host, hc := range cfg.Hosts {
		st := authHostStatus{Host: host, URL: hc.URL, Handle: hc.Handle, Default: host == cfg.DefaultHost}
		probe := &Client{base: hc.URL + "/v1", origin: hc.URL, token: hc.Token, http: c.http}
		var me struct {
			Principal struct {
				Kind   string `json:"kind"`
				Handle string `json:"handle"`
			} `json:"principal"`
		}
		if err := probe.get(ctx, "/auth/me", nil, &me); err != nil {
			st.Error = err.Error()
			probeErr[host] = err
		} else {
			st.Valid = true
			st.Handle = me.Principal.Kind + ":" + me.Principal.Handle
		}
		out = append(out, st)
	}
	dir := directoryStatus(c, cfg, out, probeErr)
	payload := map[string]any{"config": configPathLabel(), "hosts": out, "env": envOverrides()}
	if dir != nil {
		payload["directory"] = dir
	}
	return emit(payload, func() error {
		if dir != nil && !dir.broken {
			printDirectory(dir)
		}
		if len(out) == 0 {
			fmt.Println("Входов нет: clab auth login --host https://sdlc.example.com")
		}
		for _, st := range out {
			mark := " "
			if st.Default {
				mark = "*"
			}
			own := ""
			if dir != nil && dir.hostKey != "" && dir.hostKey == st.Host {
				own = " (хост проекта каталога)"
			}
			if st.Valid {
				fmt.Printf("%s %s — %s, токен действует%s\n", mark, st.Host, st.Handle, own)
			} else {
				fmt.Printf("%s %s — токен не принят: %s%s\n", mark, st.Host, st.Error, own)
			}
		}
		if dir != nil && dir.broken {
			// Битый маркер список входов не ломает: он — предупреждение под ним.
			fmt.Printf("Маркер каталога не принят: %s\n", dir.Error)
		}
		if env := envOverrides(); len(env) > 0 {
			fmt.Printf("Окружение перекрывает конфиг: %s\n", strings.Join(env, ", "))
		}
		fmt.Printf("Конфиг: %s\n", configPathLabel())
		return nil
	})
}

// directoryStatus — блок «Каталог» по маркеру; nil — маркера нет или он не
// читается (CLAB_CONFIG_DIR). Вход на хост маркера — по уже проверенному
// списку входов: второго запроса к тому же хосту нет.
func directoryStatus(c *Client, cfg Config, hosts []authHostStatus, probeErr map[string]error) *dirStatus {
	mk, err := c.marker()
	if err != nil {
		return &dirStatus{Error: err.Error(), broken: true,
			Hint: "поправьте файл маркера или назовите хост и проект явно: --host, --project"}
	}
	if mk == nil {
		return nil
	}
	d := &dirStatus{MarkerPath: mk.Path, Project: mk.Project, Host: mk.Origin, hostKey: mk.Host}
	switch {
	case c.globals.host != "":
		d.OverriddenBy, d.override = hostFromFlag, "--host "+c.globals.host
	case os.Getenv("SDLC_API_URL") != "":
		d.OverriddenBy, d.override = hostFromEnv, "SDLC_API_URL="+os.Getenv("SDLC_API_URL")
	}
	key := mk.Host
	if key == "" {
		key, d.defaultHost = cfg.DefaultHost, cfg.DefaultHost
	}
	for _, st := range hosts {
		if key != "" && st.Host == key {
			d.LoggedIn, d.Valid, d.Handle, d.Error = true, st.Valid, st.Handle, st.Error
		}
	}
	switch {
	case mk.Host == "" && d.defaultHost == "":
		d.Hint = "в маркере хоста нет, хоста по умолчанию тоже нет: допишите хост в блок — clab setup --host <адрес>"
	case mk.Host == "":
		d.Hint = "в маркере хоста нет, будет хост по умолчанию " + d.defaultHost + "; дописать хост в блок — clab setup --host <адрес>"
	case !d.LoggedIn || unauthenticated(probeErr[key]):
		// Хост недоступен по сети — вход ни при чём, подсказки нет.
		d.Hint = loginHint(mk.Origin)
	}
	return d
}

// unauthenticated — сервер не принял токен (а не, скажем, не ответил).
func unauthenticated(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Code == "unauthenticated")
}

func printDirectory(d *dirStatus) {
	fmt.Printf("Каталог: проект %s (маркер %s)\n", d.Project, d.MarkerPath)
	switch {
	case d.Host == "":
		fmt.Printf("  %s\n", d.Hint)
	case !d.LoggedIn:
		fmt.Printf("  хост %s — входа нет. %s\n", d.Host, d.Hint)
	case d.Valid:
		fmt.Printf("  хост %s — вход есть: %s, токен действует\n", d.Host, d.Handle)
	case d.Hint != "":
		fmt.Printf("  хост %s — токен не принят: %s. %s\n", d.Host, d.Error, d.Hint)
	default:
		fmt.Printf("  хост %s — токен не проверен: %s\n", d.Host, d.Error)
	}
	if d.override != "" {
		fmt.Printf("  хост маркера перекрыт: %s\n", d.override)
	}
}

func envOverrides() []string {
	out := []string{}
	for _, key := range []string{"SDLC_API_URL", "SDLC_TOKEN", "SDLC_PROJECT", "SDLC_ACTOR"} {
		if os.Getenv(key) != "" {
			out = append(out, key)
		}
	}
	return out
}

// authToken — токен для clab auth token: SDLC_TOKEN, иначе токен записи
// выбранного хоста. Хост маркера без записи — ошибка: токен другого хоста
// не подставляется.
func authToken(c *Client) (string, error) {
	if token := os.Getenv("SDLC_TOKEN"); token != "" {
		return token, nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return "", err
	}
	ch, err := chooseHost(cfg, c.globals, c.marker, true)
	if err != nil {
		return "", err
	}
	if ch.source == hostFromMarker && !ch.found {
		return "", fmt.Errorf("входа на %s нет (хост взят из маркера %s). %s", ch.origin, ch.marker.Path, loginHint(ch.origin))
	}
	if !ch.found || ch.hc.Token == "" {
		return "", fmt.Errorf("токена нет: clab auth login")
	}
	return ch.hc.Token, nil
}

// authLogout отзывает токен на сервере и убирает хост из конфига. Токен из
// окружения не трогается: он не наш.
func authLogout(ctx context.Context, c *Client, args []string) error {
	if _, err := flags("auth", args, func(*flag.FlagSet) {}); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ch, err := chooseHost(cfg, c.globals, c.marker, true)
	if err != nil {
		return err
	}
	host, hc, found := ch.host, ch.hc, ch.found
	if ch.source == hostFromMarker {
		fmt.Fprintf(os.Stderr, "Хост взят из маркера %s: %s\n", ch.marker.Path, ch.origin)
		if !found {
			return fmt.Errorf("в конфиге нет такого хоста: входа на %s нет; входы: clab auth status", ch.origin)
		}
	}
	if host == "" || !found {
		return fmt.Errorf("в конфиге нет такого хоста; входы: clab auth status")
	}
	if hc.TokenID != "" {
		probe := &Client{base: hc.URL + "/v1", origin: hc.URL, token: hc.Token, http: c.http}
		if err := probe.do(ctx, "DELETE", "/auth/tokens/"+hc.TokenID, nil, nil); err != nil {
			fmt.Fprintf(os.Stderr, "токен на сервере не отозван (%v); запись из конфига всё равно убрана\n", err)
		}
	}
	delete(cfg.Hosts, host)
	if cfg.DefaultHost == host {
		cfg.DefaultHost = ""
		for other := range cfg.Hosts {
			cfg.DefaultHost = other
			break
		}
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}
	return emit(map[string]any{"host": host, "logged_out": true}, func() error {
		fmt.Printf("Вышли с %s.\n", host)
		return nil
	})
}
