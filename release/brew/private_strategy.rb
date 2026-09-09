# typed: false
# frozen_string_literal: true

require "download_strategy"
require "utils/github"

# Скачивание ассета релиза из приватного репозитория GitHub.
#
# Формула указывает обычный адрес ассета
# (github.com/<owner>/<repo>/releases/download/<tag>/<file>), но у
# приватного репозитория он отвечает 404 даже с токеном: ассет отдаётся
# только через API по своему идентификатору. Стратегия находит ассет по
# имени в описании релиза и качает его с заголовком Authorization. Токен —
# тот же, что brew берёт для всего остального: HOMEBREW_GITHUB_API_TOKEN,
# вход `gh auth login` или keychain macOS.
#
# Файл лежит в sdlc-pipeline-backend (release/brew/) и копируется в тап
# workflow'ом релиза; править нужно там.
class GitHubPrivateReleaseDownloadStrategy < CurlDownloadStrategy
  ASSET_URL = %r{\Ahttps://github\.com/([^/]+)/([^/]+)/releases/download/([^/]+)/([^/]+)\z}

  def initialize(url, name, version, **meta)
    super
    match = ASSET_URL.match(url)
    raise ArgumentError, "не адрес ассета релиза GitHub: #{url}" unless match

    @owner, @repo, @tag, @asset = match.captures
  end

  private

  # brew сначала спрашивает заголовки адреса, потом качает оттуда, куда его
  # перенаправили. Подменяем исходный адрес адресом API до первого запроса:
  # API с токеном отвечает перенаправлением на подписанную ссылку, дальше
  # brew качает её сам, а заголовок Authorization на чужой хост не отдаёт.
  def resolve_url_basename_time_file_size(url, timeout: nil)
    meta[:headers] = [
      "Accept: application/octet-stream",
      "Authorization: Bearer #{token}",
    ]
    super(asset_api_url, timeout: timeout)
  end

  def asset_api_url
    @asset_api_url ||= begin
      release = GitHub::API.open_rest("https://api.github.com/repos/#{@owner}/#{@repo}/releases/tags/#{@tag}")
      asset = release.fetch("assets", []).find { |a| a["name"] == @asset }
      raise CurlDownloadStrategyError.new(url, "в релизе #{@tag} нет ассета #{@asset}") unless asset

      asset.fetch("url")
    end
  end

  def token
    GitHub::API.credentials ||
      raise(CurlDownloadStrategyError.new(url, "нужен доступ к GitHub: gh auth login, HOMEBREW_GITHUB_API_TOKEN или keychain"))
  end
end
