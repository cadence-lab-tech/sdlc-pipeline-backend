package workflow

import (
	"context"
	"time"
)

// Сведения об инстансе (CLAB-87): что запущено и как сюда входят. Отдаются
// открытой ручкой /v1/instance — её читают страница входа, оболочка
// интерфейса и CLI до того, как у них появился токен. Поэтому здесь только
// то, что и так видно снаружи: ни секрета клиента, ни ключа приложения.

// Способы входа в ответе /v1/instance.
const (
	LoginToken  = "token"
	LoginGitHub = "github"
	LoginOIDC   = "oidc"
)

// Режимы заведения проектов — значения SDLC_PROJECT_CREATION.
const (
	ProjectCreationOpen  = "open"
	ProjectCreationTeams = "teams"
)

// instanceOIDCTimeout ограничивает дискавери провайдера: ответ про инстанс
// не должен ждать полного таймаута клиента OIDC, пока провайдер лежит.
const instanceOIDCTimeout = 3 * time.Second

type Instance struct {
	// Версия сборки (-X main.version): vX.Y.Z у выпуска, SHA у нашего
	// инстанса, dev у локальной сборки.
	Version         string        `json:"version"`
	Tenancy         string        `json:"tenancy"`
	ProjectCreation string        `json:"project_creation"`
	Login           InstanceLogin `json:"login"`
}

// InstanceLogin — настроенные способы входа. Ненастроенного способа нет ни
// в списке, ни отдельным полем.
type InstanceLogin struct {
	Methods []string        `json:"methods"`
	GitHub  *InstanceGitHub `json:"github,omitempty"`
	OIDC    *InstanceOIDC   `json:"oidc,omitempty"`
}

// InstanceGitHub — те же поля, что у GET /v1/auth/github.
type InstanceGitHub struct {
	GitHubSettings
	Available bool `json:"available"`
}

// InstanceOIDC — те же поля, что у GET /v1/auth/oidc. Провайдер не ответил —
// способ остаётся в списке недоступным: он настроен, просто сейчас не
// работает, и страница входа должна сказать именно это.
type InstanceOIDC struct {
	OIDCSettings
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
}

// Instance собирает сведения об инстансе. Ошибок не отдаёт: недоступный
// провайдер — это состояние способа входа, а не всего ответа.
func (a *Auth) Instance(ctx context.Context, version string) Instance {
	if version == "" {
		version = "dev"
	}
	tenancy := a.tenancy
	if tenancy == "" {
		tenancy = TenancyShared
	}
	creation := ProjectCreationTeams
	if a.openProjects {
		creation = ProjectCreationOpen
	}
	out := Instance{
		Version: version, Tenancy: tenancy, ProjectCreation: creation,
		Login: InstanceLogin{Methods: []string{}},
	}
	if a.Enforced() {
		out.Login.Methods = append(out.Login.Methods, LoginToken)
	}
	if a.GitHubEnabled() {
		out.Login.Methods = append(out.Login.Methods, LoginGitHub)
		out.Login.GitHub = &InstanceGitHub{GitHubSettings: a.GitHubSettings(), Available: true}
	}
	if a.OIDCEnabled() {
		out.Login.Methods = append(out.Login.Methods, LoginOIDC)
		dctx, cancel := context.WithTimeout(ctx, instanceOIDCTimeout)
		settings, err := a.OIDCSettings(dctx)
		cancel()
		if err != nil {
			a.log.Warn("провайдер OIDC не ответил на дискавери", "issuer", a.oidc.Issuer(), "error", err)
			out.Login.OIDC = &InstanceOIDC{
				OIDCSettings: OIDCSettings{Enabled: true, Issuer: a.oidc.Issuer(), ClientID: a.oidc.ClientID()},
				Error:        "провайдер не отвечает",
			}
		} else {
			out.Login.OIDC = &InstanceOIDC{OIDCSettings: settings, Available: true}
		}
	}
	return out
}
