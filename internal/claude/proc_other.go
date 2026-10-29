//go:build !unix

package claude

import "os/exec"

func setProcessGroup(cmd *exec.Cmd) {}
