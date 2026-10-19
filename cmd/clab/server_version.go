package main

import (
	"fmt"
	"strings"
)

// Правило версий clab server (CLAB-125, q3; решение 0045):
//
//   - стек не старше v0.6.0: более ранние установки — git-клоны, их переводит
//     на пакет собственный upgrade.sh (решение 0044, п. 6);
//   - стек не новее clab: старый clab не знает, что изменилось в пакете
//     позже него, — отказ с командой обновить CLI;
//   - upgrade на версию новее clab разрешён с предупреждением: так clab
//     сам приводит стек к новой версии, а обновить CLI напоминает итог.
//
// dev, SHA и прочие не-выпуски не сравниваются, как у сверки с сервером
// (parseRelease): сборка из ветки управляет любой установкой с пакетом.

// versionLess — a старше b; обе — версии выпуска (VERSION уже проверен).
// Не разобранная версия ничему не предшествует.
func versionLess(a, b string) bool {
	va, okA := parseRelease(a)
	vb, okB := parseRelease(b)
	return okA && okB && va.less(vb)
}

// updateCLICommands — как обновить этот clab до версии v: из формулы тапа —
// brew upgrade clab (ставит последний выпуск, он не старше v), рядом команда
// на ровно v; иначе — install.sh тапа с CLAB_VERSION (решение 0038).
func updateCLICommands(v string, brew bool) string {
	if brew {
		return "    brew upgrade clab\n    # or exactly " + v + ": " + installCommand(v)
	}
	return "    " + installCommand(v)
}

// stackCompat — можно ли этому clab управлять стеком версии stack.
func stackCompat(cli, stack string, brew bool) error {
	if versionLess(stack, firstPackageVersion) {
		return fmt.Errorf("this installation runs %s, a git clone from before release packages; clab server manages %s and later.\n"+
			"  Upgrade it with its own script first, then use clab server: ./upgrade.sh %s", stack, firstPackageVersion, firstPackageVersion)
	}
	if versionLess(cli, stack) {
		return fmt.Errorf("clab %s is older than the stack %s: it does not know what changed in the package since.\n"+
			"  Update clab to %s or later, then run the command again:\n%s", cli, stack, stack, updateCLICommands(stack, brew))
	}
	return nil
}

// installCompat — правило версий clab server install (решение q2 CLAB-160,
// уточнение п. 5 решения 0045): ставится пакет не старше v0.6.0 и не новее
// clab. В отличие от upgrade, новее clab — отказ, а не предупреждение:
// данных ещё нет, отказ ничего не ломает, а старый clab не знает шагов
// установки поздних пакетов (новый ключ в .env). Без --version выбор «не
// новее clab» делает installVersion сам.
func installCompat(cli, v string, brew bool) error {
	if versionLess(v, firstPackageVersion) {
		return fmt.Errorf("%s comes from before release packages: clab server install installs %s and later.\n"+
			"  %s is installed the old way, from a clone of its tag with its own install.sh:\n"+
			"    git clone --branch %s %s.git cadence && cd cadence && ./install.sh",
			v, firstPackageVersion, v, v, cadenceRepoURL)
	}
	if versionLess(cli, v) {
		return fmt.Errorf("clab %s is older than %s: it does not know how to install a package newer than itself.\n"+
			"  Update clab to %s or later, then run the command again:\n%s", cli, v, v, updateCLICommands(v, brew))
	}
	return nil
}

func (i *installation) compatible() error {
	return stackCompat(i.e.cli, i.version, i.e.brew)
}

// targetNewerWarning — предупреждение upgrade на версию новее clab; пусто,
// если её нет.
func targetNewerWarning(cli, target string) string {
	if !versionLess(cli, target) {
		return ""
	}
	return fmt.Sprintf("clab %s is older than the target %s: the upgrade goes on, but update clab to %s afterwards - until then clab server refuses to manage the stack", cli, target, target)
}

// clabReminder — напоминание после успешного апгрейда (CLAB-148): сервер
// clab людей и агентов не обновляет, и молчать об этом нельзя. Текст тот же,
// что печатает upgrade.sh, — его ищут проверки ci/.
func clabReminder(target string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  Update clab to %s on the machines of people and agents:\n", target)
	b.WriteString("    brew upgrade clab   # installed with Homebrew\n")
	fmt.Fprintf(&b, "    %s\n", installCommand(target))
	b.WriteString("  and check with: clab version")
	return b.String()
}
