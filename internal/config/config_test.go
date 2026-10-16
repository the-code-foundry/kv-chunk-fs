package config

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var testSettings = []Setting{
	{Name: "CACHE_MIB", Default: "256", Help: "cache"},
	{Name: "API_HOST", Default: "", Help: "host"},
	{Name: "API_PORT", Default: "", Help: "port"},
	{Name: "GC_INTERVAL", Default: "1h", Help: "gc"},
	{Name: "VERIFY", Default: "false", Help: "verify"},
	{Name: "CHUNK_GLOB", Default: "*.bin,*.bin.tmp", Help: "globs"},
}

func resolve(t *testing.T, args, env []string) *Config {
	t.Helper()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	all := Register(fs, testSettings)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	c, err := Resolve(fs, all, env)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func write(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaults(t *testing.T) {
	c := resolve(t, nil, nil)
	if n, _ := c.Int("CACHE_MIB"); n != 256 {
		t.Fatalf("CACHE_MIB = %d", n)
	}
	if d, _ := c.Duration("GC_INTERVAL"); d != time.Hour {
		t.Fatalf("GC_INTERVAL = %s", d)
	}
	if got := c.List("CHUNK_GLOB"); len(got) != 2 || got[1] != "*.bin.tmp" {
		t.Fatalf("CHUNK_GLOB = %v", got)
	}
}

func TestPrecedence(t *testing.T) {
	file := write(t, "kv.env", "CACHE_MIB=100\nAPI_HOST=0.0.0.0\nAPI_PORT=9000\n")
	c := resolve(t, []string{"--config", file, "--api-port", "9200"},
		[]string{"KVCFS_API_HOST=127.0.0.1", "KVCFS_API_PORT=9100", "OTHER=x"})
	for name, want := range map[string]string{
		"CACHE_MIB": "100",       // file over default
		"API_HOST":  "127.0.0.1", // env over file
		"API_PORT":  "9200",      // flag over env and file
		"VERIFY":    "false",     // default
	} {
		if got := c.String(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if v := c.values["API_PORT"]; v.Source != "flag --api-port" {
		t.Errorf("API_PORT source %q", v.Source)
	}
}

func TestConfigFromEnv(t *testing.T) {
	file := write(t, "kv.env", "CACHE_MIB=77\n")
	c := resolve(t, nil, []string{"KVCFS_CONFIG=" + file})
	if c.String("CACHE_MIB") != "77" {
		t.Fatalf("CACHE_MIB = %q", c.String("CACHE_MIB"))
	}
}

func TestEnvFileSyntax(t *testing.T) {
	kv, err := ReadFile(write(t, "kv.env", `# comment
export KVCFS_CACHE_MIB=12
api-host = "10.0.0.1"
chunk_glob='*.bin, *.x'
VERIFY=yes   # trailing comment
`))
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"CACHE_MIB": "12", "API_HOST": "10.0.0.1", "CHUNK_GLOB": "*.bin, *.x", "VERIFY": "yes"} {
		if kv[k] != want {
			t.Errorf("%s = %q, want %q", k, kv[k], want)
		}
	}
}

func TestYAMLAndJSON(t *testing.T) {
	for name, content := range map[string]string{
		"kv.yaml": "cache_mib: 64\napi:\n  host: 127.0.0.1\n  port: 9101\nchunk_glob: [\"*.bin\", \"*.tmp\"]\nverify: true\n",
		"kv.json": `{"cache-mib": 64, "api": {"host": "127.0.0.1", "port": 9101}, "chunk_glob": ["*.bin", "*.tmp"], "verify": true}`,
		"kv.conf": "cache_mib: 64\napi_host: 127.0.0.1\napi_port: 9101\nchunk_glob: \"*.bin,*.tmp\"\nverify: true\n",
	} {
		kv, err := ReadFile(write(t, name, content))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for k, want := range map[string]string{"CACHE_MIB": "64", "API_HOST": "127.0.0.1", "API_PORT": "9101", "CHUNK_GLOB": "*.bin,*.tmp", "VERIFY": "true"} {
			if kv[k] != want {
				t.Errorf("%s: %s = %q, want %q", name, k, kv[k], want)
			}
		}
	}
}

func TestUnknownKeyAndBadValue(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	all := Register(fs, testSettings)
	_ = fs.Parse([]string{"--config", write(t, "kv.env", "NOPE=1\n")})
	if _, err := Resolve(fs, all, nil); err == nil {
		t.Fatal("unknown key accepted")
	}
	c := resolve(t, []string{"--cache-mib", "lots"}, nil)
	if _, err := c.Int("CACHE_MIB"); err == nil {
		t.Fatal("non-integer accepted")
	}
}
