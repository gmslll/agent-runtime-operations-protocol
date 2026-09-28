// Package journal provides the durable, checksummed, single-writer release journal.
package journal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

const (
	StatusPrepared      = "prepared"
	StatusRemoteSuccess = "remote_success"
	StatusCommitted     = "committed"
)

var (
	hexDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitID  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type Key struct {
	SourceCommit        string `json:"source_commit"`
	SourceTree          string `json:"source_tree"`
	LogicalVersion      string `json:"logical_version"`
	VersionPolicyDigest string `json:"version_policy_digest"`
	Destination         string `json:"destination"`
	Operation           string `json:"operation"`
	ArtifactDigest      string `json:"artifact_digest"`
}

type Entry struct {
	Sequence           uint64 `json:"sequence"`
	Key                Key    `json:"key"`
	Status             string `json:"status"`
	WorkflowDigest     string `json:"workflow_digest"`
	WorkflowLockDigest string `json:"workflow_lock_digest"`
	WorkflowIdentity   string `json:"workflow_identity"`
	RemoteDigest       string `json:"remote_digest,omitempty"`
	ExpectedOldDigest  string `json:"expected_old_digest,omitempty"`
	ResultingDigest    string `json:"resulting_digest,omitempty"`
	PreviousChecksum   string `json:"previous_checksum"`
	Checksum           string `json:"checksum"`
}

type state struct {
	SchemaVersion int     `json:"schema_version"`
	Entries       []Entry `json:"entries"`
	Checksum      string  `json:"checksum"`
}

type Store struct {
	mu        sync.Mutex
	directory string
	path      string
	lockPath  string
	lock      *os.File
	state     state
	closed    bool
}

func Open(directory string) (*Store, error) {
	if directory == "" {
		return nil, errors.New("journal directory is required")
	}
	abs, err := filepath.Abs(directory)
	if err != nil || filepath.Clean(abs) != abs {
		return nil, errors.New("journal directory is invalid")
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create journal directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil || resolved != abs {
		return nil, errors.New("journal directory must not traverse symlinks")
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		return nil, fmt.Errorf("protect journal directory: %w", err)
	}
	lockPath := filepath.Join(abs, "writer.lock")
	lockFD, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create release journal lock: %w", err)
	}
	lock := os.NewFile(uintptr(lockFD), lockPath)
	var lockStat unix.Stat_t
	if err := unix.Fstat(lockFD, &lockStat); err != nil || lockStat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = lock.Close()
		return nil, errors.New("release journal lock must be a regular file")
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, errors.New("release journal already has a writer")
	}
	store := &Store{directory: abs, path: filepath.Join(abs, "journal.json"), lockPath: lockPath, lock: lock, state: state{SchemaVersion: 1, Entries: []Entry{}}}
	if err := store.load(); err != nil {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
		return nil, err
	}
	return store, nil
}

func (store *Store) load() error {
	data, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		store.state.Checksum = stateChecksum(store.state)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read release journal: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var loaded state
	if err := decoder.Decode(&loaded); err != nil {
		return fmt.Errorf("decode release journal: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return err
	}
	if err := validateState(loaded); err != nil {
		return err
	}
	store.state = loaded
	return nil
}

func (store *Store) Append(entry Entry) (Entry, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return Entry{}, errors.New("release journal is closed")
	}
	if err := validateKey(entry.Key); err != nil {
		return Entry{}, err
	}
	if entry.Status != StatusPrepared && entry.Status != StatusRemoteSuccess && entry.Status != StatusCommitted {
		return Entry{}, errors.New("release journal status is invalid")
	}
	if err := validateEntryIdentity(entry); err != nil {
		return Entry{}, err
	}
	for _, existing := range store.state.Entries {
		if existing.Key == entry.Key && existing.Status == entry.Status {
			candidate := entry
			candidate.Sequence = existing.Sequence
			candidate.PreviousChecksum = existing.PreviousChecksum
			candidate.Checksum = existing.Checksum
			if candidate == existing {
				return existing, nil
			}
			return Entry{}, errors.New("release journal idempotency conflict")
		}
	}
	if err := validateTransition(store.state.Entries, entry); err != nil {
		return Entry{}, err
	}
	entry.Sequence = uint64(len(store.state.Entries) + 1)
	entry.PreviousChecksum = "sha256:" + strings.Repeat("0", 64)
	if len(store.state.Entries) != 0 {
		entry.PreviousChecksum = store.state.Entries[len(store.state.Entries)-1].Checksum
	}
	entry.Checksum = entryChecksum(entry)
	next := state{SchemaVersion: 1, Entries: append(append([]Entry(nil), store.state.Entries...), entry)}
	next.Checksum = stateChecksum(next)
	if err := store.write(next); err != nil {
		return Entry{}, err
	}
	store.state = next
	return entry, nil
}

func (store *Store) Entries() []Entry {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]Entry(nil), store.state.Entries...)
}

func (store *Store) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	errs := []error{unix.Flock(int(store.lock.Fd()), unix.LOCK_UN), store.lock.Close()}
	return errors.Join(errs...)
}

func (store *Store) write(next state) error {
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(store.directory, ".journal-*.tmp")
	if err != nil {
		return fmt.Errorf("create journal temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, store.path); err != nil {
		return fmt.Errorf("commit release journal: %w", err)
	}
	directory, err := os.Open(store.directory)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func validateState(value state) error {
	if value.SchemaVersion != 1 || value.Entries == nil || !hexDigest.MatchString(value.Checksum) {
		return errors.New("release journal envelope is invalid")
	}
	previous := "sha256:" + strings.Repeat("0", 64)
	seen := map[string]bool{}
	for index, entry := range value.Entries {
		if entry.Sequence != uint64(index+1) || entry.PreviousChecksum != previous || entry.Checksum != entryChecksum(entry) {
			return errors.New("release journal checksum chain is invalid")
		}
		if err := validateKey(entry.Key); err != nil {
			return err
		}
		if err := validateEntryIdentity(entry); err != nil {
			return err
		}
		identity := keyIdentity(entry.Key) + "\x00" + entry.Status
		if seen[identity] {
			return errors.New("release journal contains duplicate state")
		}
		seen[identity] = true
		if err := validateTransition(value.Entries[:index], entry); err != nil {
			return err
		}
		previous = entry.Checksum
	}
	if value.Checksum != stateChecksum(value) {
		return errors.New("release journal state checksum is invalid")
	}
	return nil
}

func validateKey(key Key) error {
	if !commitID.MatchString(key.SourceCommit) || !commitID.MatchString(key.SourceTree) || key.LogicalVersion == "" || !hexDigest.MatchString(key.VersionPolicyDigest) || key.Destination == "" || key.Operation == "" || !hexDigest.MatchString(key.ArtifactDigest) {
		return errors.New("release journal key is invalid")
	}
	if strings.ContainsAny(key.LogicalVersion+key.Destination+key.Operation, "\r\n\x00") || len(key.LogicalVersion) > 128 || len(key.Destination) > 512 || len(key.Operation) > 64 {
		return errors.New("release journal key contains unsafe text")
	}
	return nil
}

func validateEntryIdentity(entry Entry) error {
	if !hexDigest.MatchString(entry.WorkflowDigest) || !hexDigest.MatchString(entry.WorkflowLockDigest) || entry.WorkflowIdentity == "" || len(entry.WorkflowIdentity) > 512 || strings.ContainsAny(entry.WorkflowIdentity, "\r\n\x00") {
		return errors.New("release journal workflow identity is invalid")
	}
	if entry.ExpectedOldDigest != "" && !hexDigest.MatchString(entry.ExpectedOldDigest) {
		return errors.New("release journal expected digest is invalid")
	}
	return nil
}

func validateTransition(entries []Entry, candidate Entry) error {
	states := map[string]Entry{}
	for _, entry := range entries {
		if entry.Key == candidate.Key {
			states[entry.Status] = entry
		}
	}
	switch candidate.Status {
	case StatusPrepared:
		if candidate.RemoteDigest != "" || candidate.ResultingDigest != "" {
			return errors.New("prepared journal entry contains a remote result")
		}
	case StatusRemoteSuccess:
		prepared, ok := states[StatusPrepared]
		if !ok || !hexDigest.MatchString(candidate.RemoteDigest) || candidate.ResultingDigest != "" || !sameIdentity(prepared, candidate) {
			return errors.New("remote success lacks prepared state or digest")
		}
	case StatusCommitted:
		remote, ok := states[StatusRemoteSuccess]
		if !ok || !hexDigest.MatchString(candidate.RemoteDigest) || !hexDigest.MatchString(candidate.ResultingDigest) || candidate.RemoteDigest != remote.RemoteDigest || !sameIdentity(remote, candidate) {
			return errors.New("committed state lacks remote success or result digest")
		}
	}
	return nil
}

func sameIdentity(left, right Entry) bool {
	return left.Key == right.Key && left.WorkflowDigest == right.WorkflowDigest && left.WorkflowLockDigest == right.WorkflowLockDigest && left.WorkflowIdentity == right.WorkflowIdentity && left.ExpectedOldDigest == right.ExpectedOldDigest
}

func entryChecksum(entry Entry) string {
	copy := entry
	copy.Checksum = ""
	data, _ := json.Marshal(copy)
	return digest(data)
}

func stateChecksum(value state) string {
	copy := value
	copy.Checksum = ""
	data, _ := json.Marshal(copy)
	return digest(data)
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func keyIdentity(key Key) string {
	data, _ := json.Marshal(key)
	return string(data)
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("release journal has trailing JSON value")
		}
		return fmt.Errorf("release journal trailing data: %w", err)
	}
	return nil
}
