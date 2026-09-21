// Package creds is where `zae login` puts what it got back, and where every
// other command looks for it.
//
// One file, ~/.config/zae/credentials.json (XDG_CONFIG_HOME honoured), keyed
// by instance URL — because a person works against more than one instance and
// a token from one is worthless at another. The file is 0600 inside a 0700
// directory, written through a temporary file in the same directory so a
// crash mid-write cannot leave a half-parsed file where credentials were.
//
// Nothing here logs, and nothing here formats a token into an error: the file
// holds bearer tokens, and the one rule that keeps them out of scrollback,
// CI logs and bug reports is that they are never printed.
package creds

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Layout of the file. Version is there so a later format can be recognised
// rather than guessed at.
const (
	Dir     = "zae"
	Name    = "credentials.json"
	Version = 1

	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// Entry is one instance's session. Everything needed to use it and to renew
// it, and nothing else — no roles, no cached identity that could disagree
// with the token itself.
type Entry struct {
	Issuer       string    `json:"issuer"`
	ClientID     string    `json:"clientId"`
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken,omitempty"`
	Expiry       time.Time `json:"expiry,omitzero"`
	// Subject and Username are for `zae logout`/`zae whoami` to name the
	// session without decoding anything; the token stays the authority.
	Subject  string `json:"subject,omitempty"`
	Username string `json:"username,omitempty"`
	// SavedAt is when this entry was written, for a person wondering how old
	// a session is.
	SavedAt time.Time `json:"savedAt,omitzero"`
}

// Valid reports whether the access token is still usable, with a small skew
// so a token that dies in flight is renewed before it is sent.
func (e Entry) Valid(skew time.Duration) bool {
	if e.AccessToken == "" {
		return false
	}
	if e.Expiry.IsZero() {
		return true // a provider that does not say: let the instance decide
	}
	return time.Now().Add(skew).Before(e.Expiry)
}

// File is the whole document.
type File struct {
	Version   int              `json:"version"`
	Instances map[string]Entry `json:"instances"`
}

// Key normalises an instance URL into the key its entry is stored under, so
// that https://media.example.org and https://media.example.org/ are one
// session and not two.
func Key(base string) string { return strings.TrimRight(strings.TrimSpace(base), "/") }

// Path is where the credentials file lives. XDG_CONFIG_HOME wins when set —
// the same rule the rest of a Linux desktop follows — else ~/.config.
func Path() (string, error) {
	if x := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); x != "" {
		return filepath.Join(x, Dir, Name), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate a home directory for %s: %w", Name, err)
	}
	return filepath.Join(home, ".config", Dir, Name), nil
}

// Load reads the file. A missing file is an empty one, not an error: not
// having logged in yet is the normal state of a fresh install.
func Load() (*File, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &File{Version: Version, Instances: map[string]Entry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		// Say where it is and what to do; a hand-edited or truncated file must
		// not turn every command into a mystery.
		return nil, fmt.Errorf("%s is not readable as JSON (%v) — remove it and run `zae login` again", path, err)
	}
	if f.Instances == nil {
		f.Instances = map[string]Entry{}
	}
	return &f, nil
}

// Save writes the file back: 0700 directory, 0600 file, atomically.
func Save(f *File) error {
	path, err := Path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("cannot create %s: %w", dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone, and an umask can
	// widen a new one. This directory holds bearer tokens: narrow it either way.
	if err := os.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("cannot restrict %s to %o: %w", dir, dirPerm, err)
	}
	if f.Version == 0 {
		f.Version = Version
	}
	if f.Instances == nil {
		f.Instances = map[string]Entry{}
	}
	body, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode credentials: %w", err)
	}
	body = append(body, '\n')

	// A temporary file in the SAME directory, so the rename is atomic and the
	// credentials never exist under a wider mode, not even for an instant.
	tmp, err := os.CreateTemp(dir, ".credentials-*.json")
	if err != nil {
		return fmt.Errorf("cannot write in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if err := tmp.Chmod(filePerm); err != nil {
		tmp.Close()
		return fmt.Errorf("cannot restrict %s to %o: %w", tmpName, filePerm, err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("cannot write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot write %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("cannot replace %s: %w", path, err)
	}
	return nil
}

// Get returns the entry for an instance.
func Get(base string) (Entry, bool, error) {
	f, err := Load()
	if err != nil {
		return Entry{}, false, err
	}
	e, ok := f.Instances[Key(base)]
	return e, ok, nil
}

// Put stores (or replaces) one instance's session.
func Put(base string, e Entry) error {
	f, err := Load()
	if err != nil {
		return err
	}
	if e.SavedAt.IsZero() {
		e.SavedAt = time.Now()
	}
	f.Instances[Key(base)] = e
	return Save(f)
}

// Remove drops one instance's session; removed is false when there was none.
func Remove(base string) (bool, error) {
	f, err := Load()
	if err != nil {
		return false, err
	}
	key := Key(base)
	if _, ok := f.Instances[key]; !ok {
		return false, nil
	}
	delete(f.Instances, key)
	return true, Save(f)
}

// Clear removes the file itself — `zae logout --all`. Deleting beats writing
// an empty document: nothing is left on disk to read back.
func Clear() (int, error) {
	f, err := Load()
	if err != nil {
		// Unreadable and being thrown away anyway: remove it regardless.
		path, perr := Path()
		if perr != nil {
			return 0, err
		}
		if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return 0, err
		}
		return 0, nil
	}
	n := len(f.Instances)
	path, err := Path()
	if err != nil {
		return 0, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("cannot remove %s: %w", path, err)
	}
	return n, nil
}

// List returns the instance URLs with a stored session, sorted.
func List() ([]string, error) {
	f, err := Load()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(f.Instances))
	for k := range f.Instances {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}
