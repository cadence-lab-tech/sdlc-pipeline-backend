package main

import "os/exec"

// ownProcessGroup — на Windows clab server не работает (server.go), и
// команд с detach там нет.
func ownProcessGroup(*exec.Cmd) {}
