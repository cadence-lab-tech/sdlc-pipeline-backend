# Homebrew-тап Cadence Lab

```bash
brew trust cadence-lab-tech/tap          # Homebrew 6: сторонние тапы грузятся только после доверия
brew install cadence-lab-tech/tap/clab
clab auth login --host https://<адрес сервиса>
```

Ставятся `clab` и `clab-runner`. Обновление — `brew upgrade clab`.

Релизы лежат в приватном репозитории
[sdlc-pipeline-backend](https://github.com/cadence-lab-tech/sdlc-pipeline-backend),
поэтому brew нужен доступ к GitHub от участника организации. Он берёт его
сам: из `gh auth login`, из `HOMEBREW_GITHUB_API_TOKEN` или из keychain
macOS. Сам тап тоже приватный — git должен уметь его клонировать (те же
keychain или `gh auth setup-git`; либо по ssh:
`brew tap cadence-lab-tech/tap git@github.com:cadence-lab-tech/homebrew-tap.git`).

Формулу пишет workflow релиза `sdlc-pipeline-backend` по тегу `v*`;
стратегия скачивания в `lib/` копируется оттуда же. Править здесь нечего.
