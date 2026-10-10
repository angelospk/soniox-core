// Package config loads settings from the environment / a .env file and resolves
// the API keys the rest of the core needs.
package config

import (
	"bufio"
	"os"
	"strings"
)

// LoadDotEnv reads simple KEY=VALUE lines from path into the process environment
// without overriding values that are already set. Missing file is not an error.
func LoadDotEnv(path string) error {
	f, err := os.Open(path) // #nosec G304 -- fixed config path (.env), not user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := parseLine(sc.Text())
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

// parseLine splits a .env line into key/value. Returns ok=false for blanks,
// comments, and malformed lines. Surrounding quotes on the value are stripped.
func parseLine(line string) (key, val string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	i := strings.IndexByte(line, '=')
	if i <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:i])
	val = strings.TrimSpace(line[i+1:])
	val = strings.Trim(val, `"'`)
	return key, val, key != ""
}

// SonioxKey returns the Soniox API key to use, preferring a real key over a
// temporary one. Empty string means none is configured.
func SonioxKey() string {
	if k := os.Getenv("SONIOX_API_KEY"); k != "" {
		return k
	}
	return os.Getenv("SONIOX_TEMP_KEY")
}

// GeminiKey returns the Gemini API key, or "" if cleanup should be skipped.
func GeminiKey() string {
	return os.Getenv("GEMINI_API_KEY")
}
