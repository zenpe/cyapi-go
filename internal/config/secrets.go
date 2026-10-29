package config

import (
	"bufio"
	"errors"
	"os"
	"regexp"
	"strings"
)

var envKey = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// ReadSecrets reads literal KEY=VALUE lines. It never executes shell syntax or expands variables.
// A missing file is allowed for deployments using process environment variables.
func ReadSecrets(path string) (map[string]string, error) {
	values := map[string]string{}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return values, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
		return nil, errors.New("secrets file must be a regular file no larger than 64 KiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read secrets file")
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || !envKey.MatchString(key) {
			return nil, errors.New("invalid secrets file; expected literal KEY=VALUE lines")
		}
		if _, exists := values[key]; exists {
			return nil, errors.New("duplicate key in secrets file")
		}
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == 39 && value[len(value)-1] == 39)) {
			value = value[1 : len(value)-1]
		}
		values[key] = value
	}
	if scan.Err() != nil {
		return nil, errors.New("cannot parse secrets file")
	}
	return values, nil
}

// An explicitly set process environment value takes precedence, including an empty value.
func (c Config) Env(key string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return c.values[key]
}
