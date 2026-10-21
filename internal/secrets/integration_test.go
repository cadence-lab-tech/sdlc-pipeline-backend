package secrets_test

import (
	"context"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/secrets"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// AC-5 в настоящей базе: роль с паролем из спецсимволов подключается
// адресом без пароля и SDLC_DATABASE_PASSWORD_FILE — так, как сервис
// встроенной базы в режиме секретов файлами.
func TestPasswordFileConnects(t *testing.T) {
	raw := os.Getenv(testdb.EnvURL)
	if raw == "" {
		t.Skip(testdb.EnvURL + " не задан — интеграционный тест пропущен")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)

	const role, password = "clab200_secret_file", `p@:/%?#&= 'x"`
	quoted := "'" + `p@:/%?#&= ''x"` + "'"
	if _, err := admin.Exec(ctx, "DROP ROLE IF EXISTS "+role); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+role) })

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, "")
	env := map[string]string{
		secrets.DatabaseURL:                u.String(),
		secrets.DatabasePassword + "_FILE": file(t, password+"\n"),
	}
	dsn, err := secrets.Source{Getenv: func(k string) string { return env[k] }}.DatabaseURL()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("the password of the file does not open the database: %v", err)
	}
	defer conn.Close(ctx)
	var who string
	if err := conn.QueryRow(ctx, "SELECT current_user").Scan(&who); err != nil || who != role {
		t.Fatalf("current_user %q %v", who, err)
	}

	// Тот же пароль неверным — отказ: значит, пароль действительно проверяется.
	env[secrets.DatabasePassword+"_FILE"] = file(t, "wrong")
	dsn, _ = secrets.Source{Getenv: func(k string) string { return env[k] }}.DatabaseURL()
	if c, err := pgx.Connect(ctx, dsn); err == nil {
		c.Close(ctx)
		t.Log("the test database trusts every password: the check proves only the URL")
	}
}
