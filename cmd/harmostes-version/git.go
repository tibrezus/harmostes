package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	// Bot-token boundary (#480 r10 t20): enumerated child env — PATH only.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %v: %v: %s", args, err, ee.Stderr)
		}
		return "", fmt.Errorf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out)), nil
}
