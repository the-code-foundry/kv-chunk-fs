// Package config resolves settings from built-in defaults, a config file, the
// environment and command-line flags, in that order of precedence, and records
// where each value came from.
//
// Every setting has one canonical name, e.g. CACHE_MIB. It is read as
//   - a key in the config file: CACHE_MIB, cache_mib, cache-mib or KVCFS_CACHE_MIB
//     (in YAML also nested: cache: {mib: 256} flattens to CACHE_MIB);
//   - the environment variable KVCFS_CACHE_MIB;
//   - the flag --cache-mib.
//
// Config files are KEY=VALUE lines (bash style; also a systemd EnvironmentFile)
// or YAML (and therefore JSON), told apart by extension or content.
package config

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// EnvPrefix is the prefix of every environment variable.
const EnvPrefix = "KVCFS_"

// Setting describes one configurable value.
type Setting struct {
	Name    string // canonical: CACHE_MIB
	Default string
	Help    string
}

// Flag is the command-line flag for a setting: CACHE_MIB -> cache-mib.
func (s Setting) Flag() string { return strings.ToLower(strings.ReplaceAll(s.Name, "_", "-")) }

// Env is the environment variable for a setting.
func (s Setting) Env() string { return EnvPrefix + s.Name }

// Value is a resolved setting.
type Value struct {
	Setting
	Value  string
	Source string // "default", "file <path>", "env KVCFS_X", "flag --x"
}

// Config is the resolved set.
type Config struct {
	values map[string]Value
	order  []string
}

// normalize turns any accepted spelling of a key into the canonical name.
func normalize(key string) string {
	k := strings.ToUpper(strings.TrimSpace(key))
	k = strings.ReplaceAll(k, "-", "_")
	k = strings.ReplaceAll(k, ".", "_")
	return strings.TrimPrefix(k, EnvPrefix)
}

// Register adds the settings' flags to fs. The file is named by the CONFIG
// setting (flag --config, env KVCFS_CONFIG), which Register adds itself.
func Register(fs *flag.FlagSet, settings []Setting) []Setting {
	all := append([]Setting{{Name: "CONFIG", Help: "config file: KEY=VALUE lines or YAML/JSON"}}, settings...)
	for _, s := range all {
		fs.String(s.Flag(), "", fmt.Sprintf("%s (env %s, default %q)", s.Help, s.Env(), s.Default))
	}
	return all
}

// Resolve applies defaults, the config file, the environment and the flags
// that were set on fs (after fs.Parse). Unknown keys in the file are an error.
func Resolve(fs *flag.FlagSet, settings []Setting, environ []string) (*Config, error) {
	c := &Config{values: map[string]Value{}}
	known := map[string]Setting{}
	for _, s := range settings {
		known[s.Name] = s
		c.values[s.Name] = Value{Setting: s, Value: s.Default, Source: "default"}
		c.order = append(c.order, s.Name)
	}
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, EnvPrefix) {
			env[k] = v
		}
	}
	flags := map[string]string{}
	fs.Visit(func(f *flag.Flag) { flags[f.Name] = f.Value.String() })

	// which file: flag, then env
	path := env[EnvPrefix+"CONFIG"]
	if v, ok := flags["config"]; ok {
		path = v
	}
	if path != "" {
		kv, err := ReadFile(path)
		if err != nil {
			return nil, err
		}
		for k, v := range kv {
			s, ok := known[k]
			if !ok {
				return nil, fmt.Errorf("%s: unknown setting %q", path, k)
			}
			c.values[k] = Value{Setting: s, Value: v, Source: "file " + path}
		}
	}
	for _, s := range settings {
		if v, ok := env[s.Env()]; ok {
			c.values[s.Name] = Value{Setting: s, Value: v, Source: "env " + s.Env()}
		}
		if v, ok := flags[s.Flag()]; ok {
			c.values[s.Name] = Value{Setting: s, Value: v, Source: "flag --" + s.Flag()}
		}
	}
	return c, nil
}

// ReadFile reads a config file into canonical KEY -> value.
func ReadFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ext := strings.ToLower(filepath.Ext(path))
	trimmed := bytes.TrimSpace(b)
	isYAML := ext == ".yaml" || ext == ".yml" || ext == ".json" ||
		bytes.HasPrefix(trimmed, []byte("{")) || (ext != ".env" && looksLikeYAML(trimmed))
	var kv map[string]string
	if isYAML {
		kv, err = parseYAML(b)
	} else {
		kv, err = parseEnv(b)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return kv, nil
}

// looksLikeYAML: "key: value" before any "KEY=VALUE".
func looksLikeYAML(b []byte) bool {
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || line == "---" {
			continue
		}
		eq := strings.Index(line, "=")
		colon := strings.Index(line, ":")
		return colon > 0 && (eq < 0 || colon < eq)
	}
	return false
}

func parseEnv(b []byte) (map[string]string, error) {
	kv := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: want KEY=VALUE", n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i]) // trailing comment
		}
		kv[normalize(k)] = v
	}
	return kv, sc.Err()
}

func parseYAML(b []byte) (map[string]string, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	kv := map[string]string{}
	var walk func(prefix string, m map[string]any) error
	walk = func(prefix string, m map[string]any) error {
		for k, v := range m {
			name := normalize(k)
			if prefix != "" {
				name = prefix + "_" + name
			}
			switch x := v.(type) {
			case map[string]any:
				if err := walk(name, x); err != nil {
					return err
				}
			case []any:
				parts := make([]string, len(x))
				for i, e := range x {
					parts[i] = fmt.Sprint(e)
				}
				kv[name] = strings.Join(parts, ",")
			case nil:
				kv[name] = ""
			default:
				kv[name] = fmt.Sprint(x)
			}
		}
		return nil
	}
	return kv, walk("", doc)
}

// String returns a setting's value.
func (c *Config) String(name string) string { return c.values[name].Value }

// Int returns a setting as an integer.
func (c *Config) Int(name string) (int64, error) {
	v := c.values[name]
	n, err := strconv.ParseInt(strings.TrimSpace(v.Value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q (%s): not an integer", name, v.Value, v.Source)
	}
	return n, nil
}

// Float returns a setting as a float.
func (c *Config) Float(name string) (float64, error) {
	v := c.values[name]
	f, err := strconv.ParseFloat(strings.TrimSpace(v.Value), 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q (%s): not a number", name, v.Value, v.Source)
	}
	return f, nil
}

// Bool returns a setting as a boolean (1/0, true/false, yes/no, on/off).
func (c *Config) Bool(name string) (bool, error) {
	v := c.values[name]
	switch strings.ToLower(strings.TrimSpace(v.Value)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off", "":
		return false, nil
	}
	return false, fmt.Errorf("%s=%q (%s): not a boolean", name, v.Value, v.Source)
}

// Duration returns a setting as a duration ("1h", "90s"; a bare number is seconds).
func (c *Config) Duration(name string) (time.Duration, error) {
	v := c.values[name]
	s := strings.TrimSpace(v.Value)
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(n * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s=%q (%s): not a duration", name, v.Value, v.Source)
	}
	return d, nil
}

// List returns a comma-separated setting as its non-empty parts.
func (c *Config) List(name string) []string {
	var out []string
	for _, p := range strings.Split(c.values[name].Value, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Values lists every setting in registration order.
func (c *Config) Values() []Value {
	out := make([]Value, 0, len(c.order))
	for _, n := range c.order {
		out = append(out, c.values[n])
	}
	return out
}

// Dump renders the effective configuration as KEY=VALUE lines with sources.
func (c *Config) Dump() string {
	var b strings.Builder
	for _, v := range c.Values() {
		fmt.Fprintf(&b, "%s=%s\t# %s\n", v.Name, quote(v.Value), v.Source)
	}
	return b.String()
}

func quote(s string) string {
	if s == "" || strings.ContainsAny(s, " \t#\"'") {
		return strconv.Quote(s)
	}
	return s
}
