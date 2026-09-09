#!/usr/bin/env sh
#
# Формула Homebrew для clab из контрольных сумм релиза:
#
#   release/brew/formula.sh v0.1.0 checksums.txt > Formula/clab.rb
#
# Зовётся workflow'ом релиза после сборки архивов; checksums.txt — тот же,
# что лежит в релизе. Репозиторий с релизами приватный, поэтому формула
# качает через lib/private_strategy.rb, а не по прямой ссылке.
set -eu

tag="${1:?тег релиза, например v0.1.0}"
sums="${2:?путь к checksums.txt}"
repo="${CLAB_RELEASE_REPO:-cadence-lab-tech/sdlc-pipeline-backend}"
version="${tag#v}"

sum() {
  s=$(awk -v f="clab_${tag}_$1.tar.gz" '$2 == f { print $1 }' "$sums")
  [ -n "$s" ] || { echo "✗ в $sums нет суммы для $1" >&2; exit 1; }
  printf '%s' "$s"
}

darwin_arm64=$(sum darwin_arm64)
darwin_amd64=$(sum darwin_amd64)
linux_arm64=$(sum linux_arm64)
linux_amd64=$(sum linux_amd64)

base="https://github.com/$repo/releases/download/$tag"

cat <<RUBY
# typed: false
# frozen_string_literal: true

# Формулу создаёт release/brew/formula.sh в sdlc-pipeline-backend на каждый
# релиз; править руками бессмысленно — следующий релиз перезапишет.
require_relative "../lib/private_strategy"

class Clab < Formula
  desc "Cadence Lab: задачи, гейты и MCP из терминала"
  homepage "https://github.com/$repo"
  version "$version"

  on_macos do
    on_arm do
      url "$base/clab_${tag}_darwin_arm64.tar.gz",
          using: GitHubPrivateReleaseDownloadStrategy
      sha256 "$darwin_arm64"
    end
    on_intel do
      url "$base/clab_${tag}_darwin_amd64.tar.gz",
          using: GitHubPrivateReleaseDownloadStrategy
      sha256 "$darwin_amd64"
    end
  end

  on_linux do
    on_arm do
      url "$base/clab_${tag}_linux_arm64.tar.gz",
          using: GitHubPrivateReleaseDownloadStrategy
      sha256 "$linux_arm64"
    end
    on_intel do
      url "$base/clab_${tag}_linux_amd64.tar.gz",
          using: GitHubPrivateReleaseDownloadStrategy
      sha256 "$linux_amd64"
    end
  end

  def install
    bin.install "clab", "clab-runner"
  end

  test do
    assert_match "clab $tag", shell_output("#{bin}/clab version")
  end
end
RUBY
