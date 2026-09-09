# Релизы CLI

Как `clab` попадает на машины коллег. Почему так —
[решение 0032](https://github.com/cadence-lab-tech/sdlc-pipeline-docs/blob/main/decisions/0032-cli-login-device-flow.md).

## Что происходит по тегу

```bash
git tag v0.2.0 && git push origin v0.2.0
```

`.github/workflows/release.yml` собирает `clab` и `clab-runner` под
darwin/arm64, darwin/amd64, linux/amd64, linux/arm64 и windows/amd64,
кладёт архивы и `checksums.txt` в GitHub Releases, рендерит формулу
Homebrew (`brew/formula.sh`) и пушит её в тап
[cadence-lab-tech/homebrew-tap](https://github.com/cadence-lab-tech/homebrew-tap)
вместе со стратегией скачивания `brew/private_strategy.rb` и README тапа.
`clab version` отвечает именем тега.

## Три способа поставить

| | команда | что нужно |
|---|---|---|
| Homebrew (macOS, Linux) | `brew trust cadence-lab-tech/tap && brew install cadence-lab-tech/tap/clab` | доступ к GitHub у brew: `gh auth login`, `HOMEBREW_GITHUB_API_TOKEN` или keychain; `brew trust` — требование Homebrew 6 к сторонним тапам |
| скрипт с сервиса (macOS, Linux) | `curl -fsSL https://<адрес>/install.sh \| sh` | `gh auth login` или `GITHUB_TOKEN` |
| руками (и Windows) | `gh release download --repo cadence-lab-tech/sdlc-pipeline-backend --pattern 'clab_*_windows_amd64.zip'` | `gh` |

Репозиторий приватный, поэтому доступ к GitHub нужен во всех трёх: прямая
ссылка на ассет приватного релиза отвечает 404, ассет отдаётся через API
по идентификатору. Формула качает через `GitHubPrivateReleaseDownloadStrategy`
(`brew/private_strategy.rb`), скрипт `/install.sh` — тем же API с токеном
из `gh auth token` или `GITHUB_TOKEN`; оба сверяют `checksums.txt`.

Скрипт установки лежит в `internal/httpapi/install.sh` и встроен в сервис:
`GET /install.sh` подставляет в него адрес, с которого его взяли, и скрипт
в конце подсказывает `clab auth login --host <этот адрес>`. Caddy на проде
отдаёт этот путь сервису вместе с `/v1` и `/mcp`.

## Ключ для тапа

Токен прогона Actions видит только свой репозиторий, поэтому в тап
workflow пушит deploy-ключом. Завести один раз:

```bash
ssh-keygen -t ed25519 -N '' -C 'release.yml -> homebrew-tap' -f tap_deploy_key
gh repo deploy-key add tap_deploy_key.pub --repo cadence-lab-tech/homebrew-tap \
    --allow-write --title 'release.yml (sdlc-pipeline-backend)'
gh secret set TAP_DEPLOY_KEY --repo cadence-lab-tech/sdlc-pipeline-backend < tap_deploy_key
rm tap_deploy_key tap_deploy_key.pub
```

Без секрета релиз всё равно выходит, шаг «обновить тап» пропускается —
формулу тогда можно отрендерить и запушить руками:

```bash
gh release download v0.2.0 --repo cadence-lab-tech/sdlc-pipeline-backend --pattern checksums.txt
release/brew/formula.sh v0.2.0 checksums.txt > ../homebrew-tap/Formula/clab.rb
```

## Проверить формулу локально

```bash
brew trust cadence-lab-tech/tap
brew tap cadence-lab-tech/tap
brew install --verbose clab
brew test clab
```

Ещё не запушенную формулу можно проверить из локального клона тапа — brew
доверяет ему по адресу, а не по имени:

```bash
brew trust --tap file:///path/to/homebrew-tap
brew tap cadence-lab-tech/tap file:///path/to/homebrew-tap
```

`brew audit --strict clab` ругается на нестандартную стратегию — это
ожидаемо, для приватного репозитория стандартной нет.
