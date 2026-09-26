package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
	"gopkg.in/yaml.v2"
)

var (
	ErrNotFoundConfig = errors.New("NotFound Config")
)

var (
	YamlConfigPath = configFilePath("config.yml")
)

type Config struct {
	LowercaseKeywords bool                 `json:"lowercaseKeywords" yaml:"lowercaseKeywords"`
	Connections       []*database.DBConfig `json:"connections" yaml:"connections"`
	Diagnostics       DiagnosticsConfig    `json:"diagnostics" yaml:"diagnostics"`
}

// DiagnosticsConfig is validated by sqlsymbol so the internal host and the
// public language service share one rule-code and level registry.
type DiagnosticsConfig struct {
	Rules map[string]string `json:"rules" yaml:"rules"`
}

func (d DiagnosticsConfig) Validate() error {
	if len(d.Rules) == 0 {
		return nil
	}
	return sqlsymbol.DiagnosticOptions{Rules: d.Rules}.Validate()
}

// Options returns a detached copy for internal consumers that still use the
// sqlsymbol adapter directly. The language-service host path converts Rules
// into language.DiagnosticPolicy instead.
func (d DiagnosticsConfig) Options() sqlsymbol.DiagnosticOptions {
	if len(d.Rules) == 0 {
		return sqlsymbol.DiagnosticOptions{}
	}
	rules := make(map[string]string, len(d.Rules))
	for code, level := range d.Rules {
		rules[code] = level
	}
	return sqlsymbol.DiagnosticOptions{Rules: rules}
}

func (c *Config) Validate() error {
	if len(c.Connections) > 0 {
		if err := c.Connections[0].Validate(); err != nil {
			return err
		}
	}
	return c.Diagnostics.Validate()
}

func NewConfig() *Config {
	cfg := &Config{}
	cfg.LowercaseKeywords = false
	return cfg
}

func GetDefaultConfig() (*Config, error) {
	cfg := NewConfig()
	if err := cfg.Load(YamlConfigPath); err != nil {
		return nil, err
	}
	return cfg, nil
}

func GetConfig(fp string) (*Config, error) {
	cfg := NewConfig()
	expandPath, err := expand(fp)
	if err != nil {
		return nil, err
	}
	if err := cfg.Load(expandPath); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) Load(fp string) error {
	if !IsFileExist(fp) {
		return ErrNotFoundConfig
	}

	file, err := os.OpenFile(fp, os.O_RDONLY, 0666)
	if err != nil {
		return fmt.Errorf("cannot open config, %w", err)
	}
	defer file.Close()

	b, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("cannot read config, %w", err)
	}

	if err = yaml.Unmarshal(b, c); err != nil {
		return fmt.Errorf("failed unmarshal yaml, %w, %s", err, string(b))
	}

	if err := c.Validate(); err != nil {
		return fmt.Errorf("failed validation, %w", err)
	}
	return nil
}

func IsFileExist(fPath string) bool {
	_, err := os.Stat(fPath)
	return err == nil || !os.IsNotExist(err)
}

func configFilePath(fileName string) string {
	if xdgConfigHome := os.Getenv("XDG_CONFIG_HOME"); xdgConfigHome != "" {
		return filepath.Join(xdgConfigHome, "sqls", fileName)
	}

	var configDir string
	if runtime.GOOS == "darwin" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			panic(err)
		}
		configDir = filepath.Join(homeDir, ".config")
	} else {
		var err error
		configDir, err = os.UserConfigDir()
		if err != nil {
			panic(err)
		}
	}

	return filepath.Join(configDir, "sqls", fileName)
}

func expand(path string) (string, error) {
	if len(path) == 0 || path[0] != '~' {
		return path, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, path[1:]), nil
}
