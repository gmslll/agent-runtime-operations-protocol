package finalize

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type Freeze struct {
	Commit string `json:"commit"`
	Tree   string `json:"tree"`
}

func CaptureFreeze(root string, publicDestinations []string) (Freeze, error) {
	status, err := git(root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return Freeze{}, err
	}
	if status != "" {
		return Freeze{}, errors.New("RC source freeze requires a clean worktree")
	}
	if len(publicDestinations) != 0 {
		return Freeze{}, errors.New("RC source freeze cannot run after public tag or package publication")
	}
	commit, err := git(root, "rev-parse", "HEAD")
	if err != nil {
		return Freeze{}, err
	}
	tree, err := git(root, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return Freeze{}, err
	}
	return Freeze{Commit: commit, Tree: tree}, nil
}

func VerifyFreeze(root string, freeze Freeze) error {
	if len(freeze.Commit) != 40 || len(freeze.Tree) != 40 {
		return errors.New("freeze commit and tree must be full SHA-1 object IDs")
	}
	tree, err := git(root, "rev-parse", freeze.Commit+"^{tree}")
	if err != nil || tree != freeze.Tree {
		return errors.New("freeze tree does not match source commit")
	}
	return nil
}

func git(root string, arguments ...string) (string, error) {
	command := exec.Command("git", arguments...)
	command.Dir = root
	command.Env = cleanEnv(os.Environ())
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s failed: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func cleanEnv(values []string) []string {
	blocked := map[string]bool{"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true, "GIT_CONFIG_GLOBAL": true, "GIT_CONFIG_SYSTEM": true, "GIT_CONFIG_COUNT": true}
	result := []string{"GIT_CONFIG_NOSYSTEM=1"}
	for _, value := range values {
		key := strings.ToUpper(strings.SplitN(value, "=", 2)[0])
		if !blocked[key] && !strings.HasPrefix(key, "GIT_CONFIG_KEY_") && !strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			result = append(result, value)
		}
	}
	return result
}
