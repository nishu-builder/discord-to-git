package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

// Refuse an existing human checkout. This directory belongs exclusively to the bot.
func initRepo(ctx context.Context, c config, repo string) error {
	if err := os.MkdirAll(repo, 0700); err != nil {
		return err
	}
	if _, err := git(ctx, repo, "check-ref-format", "--branch", c.Branch); err != nil {
		return err
	}
	marker := filepath.Join(repo, ".git", "discord-to-git")
	if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
		entries, err := os.ReadDir(repo)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return errors.New("archive output must be empty or owned by discord-to-git")
		}
		if _, err := git(ctx, repo, "init", "-b", c.Branch); err != nil {
			return err
		}
		if err := os.WriteFile(marker, []byte("Generated archive; do not edit by hand.\n"), 0600); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := git(ctx, repo, "config", "user.name", "Discord Archive"); err != nil {
		return err
	}
	if _, err := git(ctx, repo, "config", "user.email", "discord-archive@localhost"); err != nil {
		return err
	}
	head, err := git(ctx, repo, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return err
	}
	if head != c.Branch {
		return fmt.Errorf("archive branch is %s, configured branch is %s", head, c.Branch)
	}
	return nil
}

func publish(ctx context.Context, c config, repo, stage string) (string, error) {
	entries, err := os.ReadDir(repo)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Name() != ".git" {
			if err := os.RemoveAll(filepath.Join(repo, entry.Name())); err != nil {
				return "", err
			}
		}
	}
	entries, err = os.ReadDir(stage)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if err := os.Rename(filepath.Join(stage, entry.Name()), filepath.Join(repo, entry.Name())); err != nil {
			return "", err
		}
	}
	if _, err := git(ctx, repo, "add", "--all"); err != nil {
		return "", err
	}
	status, err := git(ctx, repo, "diff", "--cached", "--name-only")
	if err != nil {
		return "", err
	}
	if status != "" {
		if _, err := git(ctx, repo, "commit", "-m", "Update Discord archive"); err != nil {
			return "", err
		}
	}
	rev, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if c.Remote != "" {
		// Retry an earlier failed push even if this snapshot made no new commit.
		if _, err := git(ctx, repo, "push", c.Remote, "HEAD:refs/heads/"+c.Branch); err != nil {
			return "", err
		}
	}
	return rev, nil
}

// Restore only this bot-owned checkout before using it as a checkpoint. A crash
// during publish may have replaced files without advancing HEAD.
func restoreCheckpoint(ctx context.Context, c config, repo string) (bool, error) {
	refs, err := git(ctx, repo, "for-each-ref", "--format=%(refname)", "refs/heads/"+c.Branch)
	if err != nil {
		return false, err
	}
	exists := false
	for _, ref := range strings.Split(refs, "\n") {
		if ref == "refs/heads/"+c.Branch {
			exists = true
		}
	}
	if !exists {
		return false, nil
	} // An unfinished first snapshot is not a baseline.
	if _, err := git(ctx, repo, "restore", "--source=HEAD", "--staged", "--worktree", "--", "."); err != nil {
		return false, err
	}
	if _, err := git(ctx, repo, "clean", "-fd"); err != nil {
		return false, err
	}
	return true, nil
}
