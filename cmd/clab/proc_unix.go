//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup — команда в своей группе процессов: Ctrl-C терминала
// (SIGINT всей группе переднего плана) до неё не доходит.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
