package addon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

// maxSecretBytes bounds one secret input file and one --secret-values document.
const maxSecretBytes = 1 << 20

// maxSecretPathLen: a secret input's path is also its valuesFrom targetPath,
// which the resource bounds at 250 characters.
const maxSecretPathLen = 250

var (
	// secretNameRe is a Secret's name: a DNS-1123 subdomain.
	secretNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	// secretKeyRe is a key in a Secret's data.
	secretKeyRe = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
)

// Secret inputs reach zae four ways, and only the first is visible to other
// local users (in the process list) and to shell history:
//
//	--set-secret path=value      the value on the command line
//	--set-secret path            asked for on the terminal, not echoed
//	--set-secret-file path=FILE  the file's content, one trailing newline trimmed
//	--secret-values FILE|-       a JSON object of dotted path → string
//
// A fifth, --secret-ref path=name[/key], sends no value at all: it points the
// input at a key of a values Secret the addon already has — one kept when the
// addon was removed with its values. The key defaults to the path, which is
// the key portal-api stores each secret input under.
//
// No message zae prints quotes a secret value: errors name the path, the file
// or the flag, never what was in it.

// secretArg is one --set-secret or --set-secret-file, in command-line order.
type secretArg struct {
	file bool
	arg  string
}

// secretFlag collects --set-secret and --set-secret-file into one ordered list,
// so a later one replaces an earlier one for the same path.
type secretFlag struct {
	args *[]secretArg
	file bool
}

func (f *secretFlag) String() string { return "" }
func (f *secretFlag) Set(v string) error {
	*f.args = append(*f.args, secretArg{file: f.file, arg: v})
	return nil
}

// secretPath validates a dotted path for a secret input: it becomes a Secret
// key, so letters, digits, _ and - between the dots, and never under the
// platform's key.
func secretPath(flagName, path string) error {
	if len(path) > maxSecretPathLen {
		return fmt.Errorf("--%s: a secret input's path is at most %d characters", flagName, maxSecretPathLen)
	}
	if !pathRe.MatchString(path) {
		return fmt.Errorf("--%s: %q is not a dotted path of letters, digits, _ and -", flagName, path)
	}
	if path == reservedKey || strings.HasPrefix(path, reservedKey+".") {
		return fmt.Errorf("--%s %s: values under %q are set by the platform", flagName, path, reservedKey)
	}
	return nil
}

// prompted lists the --set-secret paths given without a value, which are
// asked for on the terminal.
func prompted(args []secretArg) []string {
	var out []string
	for _, a := range args {
		if !a.file && !strings.Contains(a.arg, "=") {
			out = append(out, a.arg)
		}
	}
	return out
}

// checkSecretArgs validates every secret argument's shape before anything is
// read or asked: a bad path fails as usage without a prompt first.
func checkSecretArgs(args []secretArg, clear []string) error {
	set := map[string]bool{}
	for _, a := range args {
		name := "set-secret"
		if a.file {
			name = "set-secret-file"
		}
		path, value, hasValue := strings.Cut(a.arg, "=")
		if a.file && (!hasValue || value == "") {
			return fmt.Errorf("--set-secret-file wants path=FILE")
		}
		if err := secretPath(name, path); err != nil {
			return err
		}
		if !a.file && hasValue && value == "" {
			return fmt.Errorf("--set-secret %s= gives an empty value — leave out =value to be asked for it, or remove the input with zae addon upgrade --clear-secret %s", path, path)
		}
		set[path] = true
	}
	for _, p := range clear {
		if err := secretPath("clear-secret", p); err != nil {
			return err
		}
		if set[p] {
			return fmt.Errorf("%s is both set and cleared", p)
		}
	}
	return nil
}

// secretInputs assembles the secret inputs: --secret-values first, then each
// --set-secret-file and --set-secret in command-line order.
func (s *session) secretInputs(src string, args []secretArg) (map[string]string, error) {
	out := map[string]string{}
	if src != "" {
		m, err := readSecretValues(src)
		if err != nil {
			return nil, err
		}
		for k, v := range m {
			out[k] = v
		}
	}
	for _, a := range args {
		path, value, hasValue := strings.Cut(a.arg, "=")
		switch {
		case a.file:
			v, err := readSecretFile(value)
			if err != nil {
				return nil, fmt.Errorf("--set-secret-file %s: %w", path, err)
			}
			value = v
		case !hasValue:
			v, err := readSecret(s, path+" (secret input, not shown): ")
			if err != nil {
				if errors.Is(err, errInterrupted) {
					return nil, err
				}
				return nil, fmt.Errorf("--set-secret %s: %w", path, err)
			}
			value = v
		}
		if value == "" {
			return nil, fmt.Errorf("secret input %s is empty — to remove a secret input, use zae addon upgrade --clear-secret %s", path, path)
		}
		out[path] = value
	}
	return out, nil
}

// secretRef points a secret input at a key of an existing values Secret.
type secretRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// parseSecretRefs reads --secret-ref path=name[/key] arguments for the addon
// name. Only the addon's own values Secrets can be named — the operator reads
// nothing else — so a reference to anything else fails here, as usage.
func parseSecretRefs(addon string, args []string) (map[string]secretRef, error) {
	refs := map[string]secretRef{}
	prefix := "zaentrum-addon-" + addon + "-"
	for _, arg := range args {
		path, target, ok := strings.Cut(arg, "=")
		if !ok {
			return nil, fmt.Errorf("--secret-ref wants path=name or path=name/key")
		}
		if err := secretPath("secret-ref", path); err != nil {
			return nil, err
		}
		name, key, hasKey := strings.Cut(target, "/")
		if !hasKey {
			key = path
		}
		switch {
		case name == "" || key == "":
			return nil, fmt.Errorf("--secret-ref %s: %q is not name or name/key", path, target)
		case len(name) > 253 || !secretNameRe.MatchString(name):
			return nil, fmt.Errorf("--secret-ref %s: %q is not a Secret name", path, name)
		case !strings.HasPrefix(name, prefix):
			return nil, fmt.Errorf("--secret-ref %s: %s is not one of %s's values Secrets (%s…) — the operator reads no other Secret", path, name, addon, prefix)
		case len(key) > 253 || !secretKeyRe.MatchString(key):
			return nil, fmt.Errorf("--secret-ref %s: %q is not a Secret key", path, key)
		}
		refs[path] = secretRef{Name: name, Key: key}
	}
	return refs, nil
}

// readSecretFile reads one secret input from a file, trimming one trailing
// newline — the one an editor or `echo` adds.
func readSecretFile(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSecretBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading %s: %v", name, err)
	}
	if len(b) > maxSecretBytes {
		return "", fmt.Errorf("%s is larger than 1 MiB", name)
	}
	switch {
	case bytes.HasSuffix(b, []byte("\r\n")):
		b = b[:len(b)-2]
	case bytes.HasSuffix(b, []byte("\n")):
		b = b[:len(b)-1]
	}
	return string(b), nil
}

// readSecretValues reads --secret-values: one JSON object of dotted path to
// string, from a file or from stdin ("-").
func readSecretValues(src string) (map[string]string, error) {
	var r io.Reader = stdin
	name := "stdin"
	if src != "-" {
		f, err := os.Open(src)
		if err != nil {
			return nil, fmt.Errorf("--secret-values: %w", err)
		}
		defer f.Close()
		r, name = f, src
	}
	b, err := io.ReadAll(io.LimitReader(r, maxSecretBytes+1))
	if err != nil {
		return nil, fmt.Errorf("--secret-values: reading %s: %v", name, err)
	}
	if len(b) > maxSecretBytes {
		return nil, fmt.Errorf("--secret-values: %s is larger than 1 MiB", name)
	}
	return parseSecretValues(b, name)
}

// parseSecretValues decodes a secret values document. A syntax error reports
// its offset only: the decoder's own message quotes the character it choked
// on, which is part of a secret.
func parseSecretValues(b []byte, name string) (map[string]string, error) {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(b)))
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			return nil, fmt.Errorf(`--secret-values: %s is not valid JSON (at byte %d) — it is one object like {"database.password": "…"}`, name, syn.Offset)
		}
		return nil, fmt.Errorf(`--secret-values: %s is not one JSON object like {"database.password": "…"}`, name)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("--secret-values: %s holds more than one JSON value", name)
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(raw))
	for _, k := range keys {
		if err := secretPath("secret-values", k); err != nil {
			return nil, err
		}
		var v string
		if json.Unmarshal(raw[k], &v) != nil {
			return nil, fmt.Errorf("--secret-values: %s: %s is not a string", name, k)
		}
		if v == "" {
			return nil, fmt.Errorf("--secret-values: %s: %s is empty — to remove a secret input, use zae addon upgrade --clear-secret %s", name, k, k)
		}
		out[k] = v
	}
	return out, nil
}
