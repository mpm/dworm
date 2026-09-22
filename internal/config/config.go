package config

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Bind    string  `toml:"bind"`
	HostEnv HostEnv `toml:"host_env"`
}

type HostEnv struct {
	Command         []string `toml:"command"`
	RefreshInterval string   `toml:"refresh_interval"`
	Timeout         string   `toml:"timeout"`
}

func (h HostEnv) Durations() (time.Duration, time.Duration, error) {
	interval, timeout := time.Duration(0), 30*time.Second
	for _, item := range []struct {
		text string
		dest *time.Duration
	}{{h.RefreshInterval, &interval}, {h.Timeout, &timeout}} {
		if item.text == "" {
			continue
		}
		v, err := time.ParseDuration(item.text)
		if err != nil || v <= 0 {
			return 0, 0, fmt.Errorf("host_env durations must be positive Go durations")
		}
		*item.dest = v
	}
	return interval, timeout, nil
}

func Load(root string) (Config, map[string]string, error) {
	var cfg Config
	data, err := os.ReadFile(filepath.Join(root, ".dworm.config"))
	if err != nil && !os.IsNotExist(err) {
		return cfg, nil, err
	}
	if err == nil {
		if err := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(&cfg); err != nil {
			return cfg, nil, fmt.Errorf(".dworm.config: %w", err)
		}
	}
	if _, _, err := cfg.HostEnv.Durations(); err != nil {
		return cfg, nil, err
	}
	if len(cfg.HostEnv.Command) == 0 && (cfg.HostEnv.RefreshInterval != "" || cfg.HostEnv.Timeout != "") {
		return cfg, nil, fmt.Errorf("host_env.command is required")
	}
	if len(cfg.HostEnv.Command) > 0 && cfg.HostEnv.Command[0] == "" {
		return cfg, nil, fmt.Errorf("host_env.command cannot be empty")
	}
	data, err = os.ReadFile(filepath.Join(root, ".dworm.env"))
	if os.IsNotExist(err) {
		return cfg, map[string]string{}, nil
	}
	if err != nil {
		return cfg, nil, err
	}
	env, err := ParseEnv(data)
	if err != nil {
		return cfg, nil, fmt.Errorf(".dworm.env: %w", err)
	}
	return cfg, env, nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func Validate(env map[string]string) error {
	for k, v := range env {
		if !envName.MatchString(k) || strings.HasPrefix(k, "_DWORM_") || strings.ContainsRune(v, 0) {
			return fmt.Errorf("invalid environment variable name or value")
		}
	}
	return nil
}

// ParseEnv accepts line-oriented dotenv data without interpolation or execution.
func ParseEnv(data []byte) (map[string]string, error) {
	result := map[string]string{}
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(make([]byte, 4096), 1024*1024)
	line := 0
	for s.Scan() {
		line++
		text := strings.TrimSpace(s.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		key, value, ok := strings.Cut(text, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || !envName.MatchString(key) {
			return nil, fmt.Errorf("invalid assignment on line %d", line)
		}
		if strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "'") {
			quote := value[0]
			end := -1
			for i := 1; i < len(value); i++ {
				if quote == '"' && value[i] == '\\' {
					i++
					continue
				}
				if value[i] == quote {
					end = i
					break
				}
			}
			if end < 0 {
				return nil, fmt.Errorf("unterminated quote on line %d", line)
			}
			rest := strings.TrimSpace(value[end+1:])
			if rest != "" && !strings.HasPrefix(rest, "#") {
				return nil, fmt.Errorf("invalid quoted value on line %d", line)
			}
			value = value[:end+1]
			if quote == '\'' {
				value = value[1 : len(value)-1]
			} else {
				var err error
				value, err = strconv.Unquote(value)
				if err != nil {
					return nil, fmt.Errorf("invalid escape on line %d", line)
				}
			}
		} else {
			for i, c := range value {
				if c == '#' && (i == 0 || value[i-1] == ' ' || value[i-1] == '\t') {
					value = strings.TrimSpace(value[:i])
					break
				}
			}
		}
		result[key] = value
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("environment input exceeds line limit")
	}
	return result, Validate(result)
}

func Merge(layers ...map[string]string) map[string]string {
	result := map[string]string{}
	for _, layer := range layers {
		for k, v := range layer {
			result[k] = v
		}
	}
	return result
}
