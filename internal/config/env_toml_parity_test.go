package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEnvTOMLParity is the US-006 drift-lint gate. It fails the build when
// a STRATA_* env var is added without wiring through Config + envMap +
// deploy/strata.toml.example + docs/site/content/reference/env-vars.md.
//
// Source of truth (mirrors scripts/audit-env-toml-parity.sh):
//   - Env vars: STRATA_* literals in cmd/ + internal/ Go files, union'd with
//     envMap keys (covers const-indirected reads).
//   - TOML keys: koanf-tagged leaf fields of Config (via reflect).
//   - Example presence: keys parsed from deploy/strata.toml.example (both
//     live + commented `key = value` lines, including nested [a.b.c] sections).
//   - Docs presence: rows in docs/site/content/reference/env-vars.md whose
//     last column carries a TOML key (or the em-dash `—` for exempt rows).
func TestEnvTOMLParity(t *testing.T) {
	root := repoRoot(t)

	envVars := collectEnvVars(t, root)
	tomlKeys := collectTOMLExampleKeys(t, filepath.Join(root, "deploy", "strata.toml.example"))
	docRows := collectDocRows(t, filepath.Join(root, "docs", "site", "content", "reference", "env-vars.md"))
	configKeys := collectConfigKeys(reflect.TypeFor[Config](), "")

	configKeySet := map[string]bool{}
	for _, k := range configKeys {
		configKeySet[k] = true
	}

	// (a) Every non-exempt env var is in envMap.
	for _, env := range sortedKeys(envVars) {
		if IsExempt(env) {
			continue
		}
		if _, ok := envMap[env]; !ok {
			t.Errorf("%s has no Config struct tag — add field + envMap entry or add to exempt list (internal/config/exempt_env_vars.go)", env)
		}
	}

	// (b) Every envMap value points at a real Config koanf path.
	for env, key := range envMap {
		if !configKeySet[key] {
			t.Errorf("envMap[%s] = %q has no matching koanf-tagged field on Config", env, key)
		}
	}

	// (c) Every Config koanf leaf has a commented or live line in the TOML
	// example.
	for _, key := range configKeys {
		if !tomlKeys[key] {
			t.Errorf("deploy/strata.toml.example is missing key %q (Config field has no commented or live line)", key)
		}
	}

	// (d) Every non-exempt env var has a row in env-vars.md with the
	// expected TOML key. Exempt env vars that appear in the docs MUST show
	// the em-dash placeholder so the column stays well-formed.
	for _, env := range sortedKeys(envVars) {
		row, present := docRows[env]
		if !present {
			if IsExempt(env) {
				continue
			}
			t.Errorf("%s missing from docs/site/content/reference/env-vars.md (add a row with the TOML key)", env)
			continue
		}
		if IsExempt(env) {
			if row != "—" && row != "-" {
				t.Errorf("%s is exempt — docs/site/content/reference/env-vars.md should carry the em-dash placeholder (got %q)", env, row)
			}
			continue
		}
		expected := envMap[env]
		if row != expected {
			t.Errorf("%s docs row has TOML key %q, expected %q", env, row, expected)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		path := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(path); err == nil {
			if strings.Contains(string(data), "module github.com/danchupin/strata\n") {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repo root (go.mod with module github.com/danchupin/strata) not found above %s", dir)
		}
		dir = parent
	}
}

var (
	reEnvCall   = regexp.MustCompile(`\("(STRATA_[A-Z0-9_]+)"`)
	reEnvAssign = regexp.MustCompile(`=\s+"(STRATA_[A-Z0-9_]+)"`)
)

func collectEnvVars(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, sub := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, sub), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range reEnvCall.FindAllStringSubmatch(string(data), -1) {
				if !strings.HasSuffix(m[1], "_") {
					out[m[1]] = true
				}
			}
			for _, m := range reEnvAssign.FindAllStringSubmatch(string(data), -1) {
				if !strings.HasSuffix(m[1], "_") {
					out[m[1]] = true
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", sub, err)
		}
	}
	for k := range envMap {
		out[k] = true
	}
	return out
}

// collectConfigKeys walks the Config type and returns every dotted koanf
// leaf path. Substructs are descended; primitives (and time.Duration,
// which is an int64 alias) terminate.
func collectConfigKeys(t reflect.Type, prefix string) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("koanf")
		if tag == "" {
			continue
		}
		key := tag
		if prefix != "" {
			key = prefix + "." + tag
		}
		if f.Type.Kind() == reflect.Struct {
			out = append(out, collectConfigKeys(f.Type, key)...)
			continue
		}
		out = append(out, key)
	}
	return out
}

var (
	reTOMLSection = regexp.MustCompile(`^\[([a-z_][a-z0-9_.]*)\]`)
	reTOMLKV      = regexp.MustCompile(`^\s*#?\s*([a-z_][a-z0-9_]*)\s*=`)
)

func collectTOMLExampleKeys(t *testing.T, path string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]bool{}
	section := ""
	for line := range strings.SplitSeq(string(data), "\n") {
		if m := reTOMLSection.FindStringSubmatch(line); m != nil {
			section = m[1]
			continue
		}
		if m := reTOMLKV.FindStringSubmatch(line); m != nil {
			key := m[1]
			if section != "" {
				key = section + "." + key
			}
			out[key] = true
		}
	}
	return out
}

// collectDocRows scans the reference page and returns env-var -> last-column
// cell. A row is recognised by the leading `| `STRATA_X` |` pattern; the
// row identifier(s) are extracted from the first pipe-delimited cell only
// (Test-only rows list several names per row via "A / B"). The TOML cell
// is the final column with surrounding whitespace + backticks trimmed.
func collectDocRows(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]string{}
	reTok := regexp.MustCompile("`(STRATA_[A-Z0-9_]+)`")
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "| `STRATA_") {
			continue
		}
		cells := strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|"), "|")
		if len(cells) < 2 {
			continue
		}
		last := strings.TrimSpace(cells[len(cells)-1])
		last = strings.Trim(last, "`")
		matches := reTok.FindAllStringSubmatch(cells[0], -1)
		if len(matches) == 0 {
			continue
		}
		for _, mm := range matches {
			if _, ok := out[mm[1]]; !ok {
				out[mm[1]] = last
			}
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
