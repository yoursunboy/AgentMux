package project

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
)

// GitInitFunc initialises a Git repository in dir.
//
// It is a plain function type rather than an interface so that a test can
// substitute a stub without a type declaration, and so that the service has
// exactly one collaborator to reason about.
type GitInitFunc func(ctx context.Context, dir string) error

// ExecGitInit runs the real git binary.
//
// The command's working directory is the project directory and nothing else.
// Initialising Git one level up would create a repository in the collection
// folder, which is precisely the mistake the project model exists to prevent.
func ExecGitInit(ctx context.Context, dir string) error {
	binary, err := exec.LookPath("git")
	if err != nil {
		return wrapError(err, CodeGitUnavailable,
			"git was not found on PATH; install Git, or create the project without Initialize Git")
	}

	cmd := exec.CommandContext(ctx, binary, "init")
	cmd.Dir = dir

	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return wrapError(err, CodeGitInitFailed, "git init failed in %s: %s", dir, detail)
	}
	return nil
}
