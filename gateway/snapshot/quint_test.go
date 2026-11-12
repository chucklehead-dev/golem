package snapshot

import "os/exec"

func lookQuint() (string, error) { return exec.LookPath("quint") }
