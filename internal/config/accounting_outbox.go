package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

type PriceRate struct {
	Provider      string  `yaml:"provider" json:"provider"`
	Model         string  `yaml:"model" json:"model"`
	Tier          string  `yaml:"tier" json:"tier"`
	MinContext    int64   `yaml:"min-context" json:"min_context"`
	MaxContext    int64   `yaml:"max-context" json:"max_context"`
	Currency      string  `yaml:"currency" json:"currency"`
	Input         *string `yaml:"input" json:"input,omitempty"`
	Output        *string `yaml:"output" json:"output,omitempty"`
	CacheRead     *string `yaml:"cache-read" json:"cache_read,omitempty"`
	CacheCreation *string `yaml:"cache-creation" json:"cache_creation,omitempty"`
	Reasoning     *string `yaml:"reasoning" json:"reasoning,omitempty"`
}

type PriceAlias struct {
	Provider       string `yaml:"provider" json:"provider"`
	Model          string `yaml:"model" json:"model"`
	TargetProvider string `yaml:"target-provider" json:"target_provider"`
	TargetModel    string `yaml:"target-model" json:"target_model"`
}

type PricingConfig struct {
	LiteLLMCatalogPath string `yaml:"litellm-catalog-path" json:"litellm-catalog-path"`
	// LiteLLMCatalogURL, when set, is downloaded at startup and every LiteLLMCatalogRefreshHours.
	LiteLLMCatalogURL          string       `yaml:"litellm-catalog-url" json:"litellm-catalog-url"`
	LiteLLMCatalogRefreshHours int          `yaml:"litellm-catalog-refresh-hours" json:"litellm-catalog-refresh-hours"`
	Overrides                  []PriceRate  `yaml:"overrides" json:"overrides"`
	Aliases                    []PriceAlias `yaml:"aliases" json:"aliases"`
}

type LiteLLMExporterConfig struct {
	Enabled     bool                       `yaml:"enabled" json:"enabled"`
	URL         string                     `yaml:"url" json:"url"`
	AdminKeyEnv string                     `yaml:"admin-key-env" json:"admin-key-env"`
	Clients     map[string]LiteLLMIdentity `yaml:"clients" json:"clients"`
}

type LiteLLMIdentity struct {
	KeyHash string `yaml:"key-hash" json:"key-hash"`
	UserID  string `yaml:"user-id" json:"user-id"`
	TeamID  string `yaml:"team-id" json:"team-id"`
}

type IDVExporterConfig struct {
	Enabled       bool   `yaml:"enabled" json:"enabled"`
	URL           string `yaml:"url" json:"url"`
	Dataset       string `yaml:"dataset" json:"dataset"`
	PricesDataset string `yaml:"prices-dataset" json:"prices-dataset"`
	TokenEnv      string `yaml:"token-env" json:"token-env"`
	// ExportProject sends the project column, which receivers without it reject.
	ExportProject bool `yaml:"export-project" json:"export-project"`
}

var idvDatasetPattern = regexp.MustCompile(`^[a-z0-9_-]+/[a-z0-9_]+$`)

type AccountingOutboxConfig struct {
	LiteLLM         LiteLLMExporterConfig `yaml:"litellm" json:"litellm"`
	IDV             IDVExporterConfig     `yaml:"idv" json:"idv"`
	Pricing         PricingConfig         `yaml:"pricing" json:"pricing"`
	Enabled         bool                  `yaml:"enabled" json:"enabled"`
	DataPath        string                `yaml:"data-path" json:"data-path"`
	QueueCapacity   int                   `yaml:"queue-capacity" json:"queue-capacity"`
	MaxDiskMB       int                   `yaml:"max-disk-mb" json:"max-disk-mb"`
	MaxEvents       int                   `yaml:"max-events" json:"max-events"`
	RetentionHours  int                   `yaml:"retention-hours" json:"retention-hours"`
	ShutdownSeconds int                   `yaml:"shutdown-seconds" json:"shutdown-seconds"`
	// ProjectPaths maps a client working directory prefix to its project name.
	ProjectPaths map[string]string `yaml:"project-paths" json:"project-paths"`
}

func (c AccountingOutboxConfig) Defaults() AccountingOutboxConfig {
	if c.QueueCapacity == 0 {
		c.QueueCapacity = 512
	}
	if c.MaxDiskMB == 0 {
		c.MaxDiskMB = 256
	}
	if c.MaxEvents == 0 {
		c.MaxEvents = 100000
	}
	if c.RetentionHours == 0 {
		c.RetentionHours = 168
	}
	if c.ShutdownSeconds == 0 {
		c.ShutdownSeconds = 5
	}
	if c.Pricing.LiteLLMCatalogRefreshHours == 0 {
		c.Pricing.LiteLLMCatalogRefreshHours = 6
	}
	return c
}

func (c AccountingOutboxConfig) Validate() error {
	c = c.Defaults()
	if c.IDV.Enabled {
		u, err := url.Parse(c.IDV.URL)
		if !c.Enabled || err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !idvDatasetPattern.MatchString(c.IDV.Dataset) {
			return fmt.Errorf("IDV exporter requires an enabled outbox, HTTP(S) URL without credentials or query, and an owner/name dataset")
		}
		if c.IDV.PricesDataset != "" && (!idvDatasetPattern.MatchString(c.IDV.PricesDataset) || c.IDV.PricesDataset == c.IDV.Dataset) {
			return fmt.Errorf("IDV prices-dataset must be a different owner/name dataset")
		}
	}
	if c.LiteLLM.Enabled {
		u, err := url.Parse(c.LiteLLM.URL)
		if !c.Enabled || err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimSpace(c.LiteLLM.AdminKeyEnv) == "" {
			return fmt.Errorf("LiteLLM exporter requires an enabled outbox, HTTP(S) URL without credentials or query, and admin-key-env")
		}
	}
	if c.Pricing.LiteLLMCatalogURL != "" {
		u, err := url.Parse(c.Pricing.LiteLLMCatalogURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || c.Pricing.LiteLLMCatalogRefreshHours < 1 || c.Pricing.LiteLLMCatalogRefreshHours > 8760 {
			return fmt.Errorf("invalid observability.accounting-outbox.pricing catalog URL or refresh interval")
		}
	}
	if c.Enabled && strings.TrimSpace(c.DataPath) == "" {
		return fmt.Errorf("observability.accounting-outbox.data-path is required when enabled")
	}
	if c.QueueCapacity < 1 || c.QueueCapacity > 4096 || c.MaxDiskMB < 8 || c.MaxDiskMB > 4096 || c.MaxEvents < 1 || c.MaxEvents > 1000000 || c.RetentionHours < 1 || c.RetentionHours > 8760 || c.ShutdownSeconds < 1 || c.ShutdownSeconds > 30 {
		return fmt.Errorf("invalid observability.accounting-outbox limits")
	}
	return nil
}
