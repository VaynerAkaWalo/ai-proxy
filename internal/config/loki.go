package config

import (
	"fmt"
	"net/url"
	"strings"
)

// LokiConfig ships selected logs to a Grafana Cloud (Loki) push endpoint.
type LokiConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// URL is the Loki base URL, without the /loki/api/v1/push path.
	URL string `yaml:"url" json:"url"`
	// UserID is the Grafana Cloud Loki instance ID used as the basic auth user.
	UserID string `yaml:"user-id" json:"user-id"`
	// TokenEnv names the environment variable holding a token with logs:write scope.
	TokenEnv string `yaml:"token-env" json:"token-env"`
	// MinLevel is the lowest severity always shipped: warn (default), error or info.
	MinLevel string `yaml:"min-level" json:"min-level"`
	// StartupSeconds ships every info log emitted this long after process start. Default 60.
	StartupSeconds int               `yaml:"startup-seconds" json:"startup-seconds"`
	Labels         map[string]string `yaml:"labels" json:"labels"`
}

func (c LokiConfig) Defaults() LokiConfig {
	if c.TokenEnv == "" {
		c.TokenEnv = "GRAFANA_CLOUD_LOGS_TOKEN"
	}
	if c.MinLevel == "" {
		c.MinLevel = "warn"
	}
	if c.StartupSeconds == 0 {
		c.StartupSeconds = 60
	}
	return c
}

func (c LokiConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	c = c.Defaults()
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || strings.TrimSpace(c.UserID) == "" {
		return fmt.Errorf("observability.loki requires an HTTP(S) url without credentials and a user-id")
	}
	switch c.MinLevel {
	case "info", "warn", "error":
	default:
		return fmt.Errorf("observability.loki.min-level must be info, warn or error")
	}
	if c.StartupSeconds < 0 || c.StartupSeconds > 3600 {
		return fmt.Errorf("observability.loki.startup-seconds must be between 0 and 3600")
	}
	return nil
}
