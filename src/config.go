package main

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/urfave/cli/v3"
)

const (
	arrayAppendPlaceholder = "..."
)

type Config struct {
	ProjectRoot string `toml:"-"`

	WithoutCABundle bool `toml:"withoutCABundle"`

	Defaults ConfigDefaults  `toml:"defaults"`
	Services []ConfigService `toml:"services"`
}

type ConfigDefaults struct {
	GOOS              string    `toml:"GOOS"`
	GOARCH            string    `toml:"GOARCH"`
	Tags              *[]string `toml:"tags"`
	AdditionalFlags   *[]string `toml:"additionalFlags"`
	WithoutTimeTZData bool      `toml:"withoutTimeTZData"`
}

type ConfigService struct {
	ConfigDefaults
	Name    string `toml:"name"`
	Package string `toml:"package"`
}

func loadConfig(c *cli.Command) (Config, error) {
	path := c.String("config")

	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := toml.Unmarshal(raw, &config); err != nil {
		return Config{}, fmt.Errorf("failed to unmarshal config file: %w", err)
	}

	config.ProjectRoot = filepath.Dir(path)

	serviceNameSet := make(map[string]struct{})
	for _, service := range config.Services {
		if _, ok := serviceNameSet[service.Name]; ok {
			return Config{}, fmt.Errorf("service name %s is not unique", service.Name)
		}
		serviceNameSet[service.Name] = struct{}{}
	}

	return config, nil
}

func (c ConfigDefaults) merge(other ConfigDefaults) ConfigDefaults {
	return ConfigDefaults{
		GOOS:              cmp.Or(c.GOOS, other.GOOS),
		GOARCH:            cmp.Or(c.GOARCH, other.GOARCH),
		Tags:              mergeStringSlice(c.Tags, other.Tags),
		AdditionalFlags:   mergeStringSlice(c.AdditionalFlags, other.AdditionalFlags),
		WithoutTimeTZData: cmp.Or(c.WithoutTimeTZData, other.WithoutTimeTZData),
	}
}

func mergeStringSlice(a, b *[]string) *[]string {
	if b == nil {
		return a
	}
	if len(*b) == 0 {
		return nil
	}
	if a != nil && (*b)[0] == arrayAppendPlaceholder {
		merged := append(*a, (*b)[1:]...)
		return &merged
	}
	return b
}
