package main

import (
	"fmt"
	"os/exec"
	"strings"
)

func gitOutput(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %v: %v: %s", args, err, ee.Stderr)
		}
		return "", fmt.Errorf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out)), nil
}
