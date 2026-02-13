package api

import (
	"bufio"
	"os"
	"strings"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// ParseEnvVar parses an environment variable in Docker-style format:
// "KEY=VALUE" (inline value) or "KEY" (read from host environment).
func ParseEnvVar(spec string) (string, string, error) {
	eqIdx := strings.Index(spec, "=")

	var name, value string
	if eqIdx == -1 {
		name = strings.TrimSpace(spec)
		if err := validateEnvName(name); err != nil {
			return "", "", err
		}
		v, ok := os.LookupEnv(name)
		if !ok {
			return "", "", errx.With(ErrEnvVarNotSet, " $%s", name)
		}
		value = v
	} else {
		name = strings.TrimSpace(spec[:eqIdx])
		if err := validateEnvName(name); err != nil {
			return "", "", err
		}
		value = spec[eqIdx+1:]
	}

	return name, value, nil
}

// ParseEnvFile parses an env file with one variable per line using the same
// semantics as ParseEnvVar. Blank lines and lines starting with '#' are ignored.
func ParseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errx.Wrap(ErrReadEnvFile, err)
	}
	defer f.Close()

	result := make(map[string]string)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		name, value, err := ParseEnvVar(line)
		if err != nil {
			return nil, errx.With(ErrEnvFileLine, " %s:%d: %v", path, lineNo, err)
		}
		result[name] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, errx.Wrap(ErrReadEnvFile, err)
	}

	return result, nil
}

// ParseEnvs merges env files and explicit env flags into one map.
// Later values override earlier ones:
// 1) env files in provided order, then 2) explicit env specs in provided order.
func ParseEnvs(envSpecs []string, envFiles []string) (map[string]string, error) {
	if len(envSpecs) == 0 && len(envFiles) == 0 {
		return nil, nil
	}

	result := make(map[string]string)

	for _, path := range envFiles {
		fileEnv, err := ParseEnvFile(path)
		if err != nil {
			return nil, err
		}
		for k, v := range fileEnv {
			result[k] = v
		}
	}

	for _, spec := range envSpecs {
		name, value, err := ParseEnvVar(spec)
		if err != nil {
			return nil, err
		}
		result[name] = value
	}

	return result, nil
}

func validateEnvName(name string) error {
	if name == "" {
		return ErrEnvNameEmpty
	}
	if strings.Contains(name, "=") {
		return errx.With(ErrEnvNameInvalid, " %q", name)
	}
	if strings.ContainsRune(name, 0) {
		return errx.With(ErrEnvNameInvalid, " %q", name)
	}
	return nil
}
