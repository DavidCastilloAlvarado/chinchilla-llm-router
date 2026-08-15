// Package envfile loads KEY=VALUE pairs from a .env file into the process
// environment.
//
// Precedence: when a .env file is present, its values take priority over
// existing (system) environment variables; variables absent from the file
// keep their system values. When the file is absent, the system environment
// is used as-is.
//
// The file is read once at startup; it is not watched for changes.
package envfile

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// DefaultPath is the file LoadDefault reads from the working directory.
const DefaultPath = ".env"

// Load reads the .env file at path and sets every variable into the process
// environment, overriding existing values. It returns the number of
// variables loaded.
//
// Line syntax:
//
//	KEY=VALUE
//	export KEY=VALUE
//	# comment (whole line)
//
// Blank lines and comment lines are ignored. A single pair of matching
// single or double quotes around VALUE is stripped. Values are literal: no
// escape sequences, no ${VAR} interpolation, no inline comments.
func Load(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	n := 0
	lineNo := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return n, fmt.Errorf("%s:%d: expected KEY=VALUE", path, lineNo)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return n, fmt.Errorf("%s:%d: empty variable name", path, lineNo)
		}
		if err := os.Setenv(key, stripQuotes(strings.TrimSpace(value))); err != nil {
			return n, fmt.Errorf("%s:%d: set %s: %w", path, lineNo, key, err)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		return n, fmt.Errorf("%s: %w", path, err)
	}
	return n, nil
}

// LoadDefault loads DefaultPath from the current working directory. A
// missing file is not an error: it returns (0, false, nil) and the system
// environment is used as-is.
func LoadDefault() (loaded int, found bool, err error) {
	n, err := Load(DefaultPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return n, true, nil
}

// stripQuotes removes one pair of matching surrounding quotes.
func stripQuotes(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
