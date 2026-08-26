// Package config собирает настройки сервиса из окружения.
//
// Всё конфигурируется переменными с префиксом SDLC_, потому что сервис
// разворачивается в контейнере: файл конфигурации там пришлось бы монтировать
// отдельно, а секрет подключения к базе всё равно приходит окружением.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// HTTP
	HTTPAddr        string
	ShutdownTimeout time.Duration
	RequestTimeout  time.Duration

	// База
	DatabaseURL    string
	MaxConns       int32
	ConnectTimeout time.Duration

	// Старт
	AutoMigrate bool
	// Имена сидов из migrations/seed без расширения, в порядке применения.
	// Применяются только на пустой базе — сиды не идемпотентны.
	Seeds []string

	// Движок
	// StrictGates=false: гейт нереализованного типа отдаёт skipped и пропускает
	// переход. Это режим отладки цикла, пока фазы 2-3 не написаны.
	// StrictGates=true: такой гейт валит переход. Значение для продакшена.
	StrictGates bool

	// Аутентификации нет (решение 0010): актор приезжает заголовком
	// X-SDLC-Actor, при его отсутствии подставляется DefaultActor.
	DefaultActor string

	LogLevel string
	LogJSON  bool
	Env      string
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:        env("SDLC_HTTP_ADDR", ":8080"),
		ShutdownTimeout: 15 * time.Second,
		RequestTimeout:  30 * time.Second,
		DatabaseURL:     env("SDLC_DATABASE_URL", ""),
		MaxConns:        10,
		ConnectTimeout:  30 * time.Second,
		AutoMigrate:     true,
		StrictGates:     false,
		DefaultActor:    env("SDLC_DEFAULT_ACTOR", "anonymous"),
		LogLevel:        env("SDLC_LOG_LEVEL", "info"),
		Env:             env("SDLC_ENV", "development"),
	}

	if c.DatabaseURL == "" {
		return c, fmt.Errorf("SDLC_DATABASE_URL не задан")
	}

	var err error
	if c.AutoMigrate, err = envBool("SDLC_AUTO_MIGRATE", c.AutoMigrate); err != nil {
		return c, err
	}
	if c.StrictGates, err = envBool("SDLC_STRICT_GATES", c.StrictGates); err != nil {
		return c, err
	}
	if c.LogJSON, err = envBool("SDLC_LOG_JSON", c.Env != "development"); err != nil {
		return c, err
	}
	if c.ShutdownTimeout, err = envDuration("SDLC_SHUTDOWN_TIMEOUT", c.ShutdownTimeout); err != nil {
		return c, err
	}
	if c.RequestTimeout, err = envDuration("SDLC_REQUEST_TIMEOUT", c.RequestTimeout); err != nil {
		return c, err
	}
	if c.ConnectTimeout, err = envDuration("SDLC_DB_CONNECT_TIMEOUT", c.ConnectTimeout); err != nil {
		return c, err
	}
	if n, err := envInt("SDLC_DB_MAX_CONNS", int(c.MaxConns)); err != nil {
		return c, err
	} else {
		c.MaxConns = int32(n)
	}

	for _, s := range strings.Split(env("SDLC_SEED", ""), ",") {
		if s = strings.TrimSpace(s); s != "" {
			c.Seeds = append(c.Seeds, s)
		}
	}

	return c, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Errorf("%s: %q не булево значение", key, v)
	}
	return b, nil
}

func envInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, fmt.Errorf("%s: %q не целое число", key, v)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def, fmt.Errorf("%s: %q не длительность (нужно 15s, 2m)", key, v)
	}
	return d, nil
}
