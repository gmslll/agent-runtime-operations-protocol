package pullworker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FileCredentialSource reloads a private regular file on every request so a
// deployment can rotate the worker credential without restarting the worker.
type FileCredentialSource struct {
	Path string
}

func (source FileCredentialSource) Credential(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(source.Path)
	if err != nil || absolute == string(filepath.Separator) {
		return "", errors.New("invalid worker credential file")
	}
	if err := rejectSymlinkPath(absolute); err != nil {
		return "", err
	}
	file, err := os.Open(absolute)
	if err != nil {
		return "", errors.New("worker credential unavailable")
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0o077 != 0 || before.Size() < 1 || before.Size() > 4096 {
		return "", errors.New("worker credential file is not private regular data")
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) == 0 || len(data) > 4096 {
		return "", errors.New("worker credential unavailable")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || before.Mode() != after.Mode() {
		return "", errors.New("worker credential changed while reading")
	}
	value := strings.TrimSuffix(string(data), "\n")
	value = strings.TrimSuffix(value, "\r")
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, " \t\r\n,") {
		return "", errors.New("worker credential is invalid")
	}
	return value, nil
}

func rejectSymlinkPath(path string) error {
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator)), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return errors.New("worker credential unavailable")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("worker credential path contains a symlink")
		}
	}
	return nil
}
