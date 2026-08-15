package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func writeEnv(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing .env: %v", err)
	}
	return path
}

func TestLoad_ParsesAllForms(t *testing.T) {
	path := writeEnv(t, `
# a comment line

PLAIN=hello
QUOTED_DOUBLE="world"
QUOTED_SINGLE='single'
WITH_EQUALS=abc=def=ghi
export EXPORTED=yes
SPACES =  padded  
`)
	n, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n != 6 {
		t.Fatalf("loaded = %d, want 6", n)
	}
	want := map[string]string{
		"PLAIN":         "hello",
		"QUOTED_DOUBLE": "world",
		"QUOTED_SINGLE": "single",
		"WITH_EQUALS":   "abc=def=ghi",
		"EXPORTED":      "yes",
		"SPACES":        "padded",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestLoad_OverridesSystemEnv(t *testing.T) {
	const key = "ENVFILE_TEST_OVERRIDE"
	t.Setenv(key, "from-system")
	path := writeEnv(t, key+"=from-dotenv\n")
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := os.Getenv(key); got != "from-dotenv" {
		t.Fatalf("%s = %q, want from-dotenv (.env takes priority)", key, got)
	}
}

func TestLoad_KeepsSystemEnvForMissingKeys(t *testing.T) {
	const key = "ENVFILE_TEST_KEPT"
	t.Setenv(key, "kept")
	path := writeEnv(t, "OTHER=x\n")
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := os.Getenv(key); got != "kept" {
		t.Fatalf("%s = %q, want kept", key, got)
	}
}

func TestLoad_MalformedLine(t *testing.T) {
	path := writeEnv(t, "GOOD=1\nNOT A LINE\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for malformed line")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.env"))
	if !os.IsNotExist(err) {
		t.Fatalf("err = %v, want IsNotExist", err)
	}
}

func TestLoadDefault_MissingFileIsNotError(t *testing.T) {
	t.Chdir(t.TempDir())
	n, found, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}
	if found || n != 0 {
		t.Fatalf("found=%v n=%d, want false/0", found, n)
	}
}

func TestLoadDefault_Found(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(DefaultPath, []byte("A=1\nB=2\n"), 0o600); err != nil {
		t.Fatalf("writing .env: %v", err)
	}
	n, found, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}
	if !found || n != 2 {
		t.Fatalf("found=%v n=%d, want true/2", found, n)
	}
	if os.Getenv("A") != "1" || os.Getenv("B") != "2" {
		t.Fatal("variables not set")
	}
}
