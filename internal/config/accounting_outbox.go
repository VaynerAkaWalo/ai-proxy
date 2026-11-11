package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
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
	LiteLLMCatalogPath         string       `yaml:"litellm-catalog-path" json:"litellm-catalog-path"`
	LiteLLMCatalogURL          string       `yaml:"litellm-catalog-url" json:"litellm-catalog-url"`
	LiteLLMCatalogRefreshHours int          `yaml:"litellm-catalog-refresh-hours" json:"litellm-catalog-refresh-hours"`
	Overrides                  []PriceRate  `yaml:"overrides" json:"overrides"`
	Aliases                    []PriceAlias `yaml:"aliases" json:"aliases"`
}

type AnalyticsExporterConfig struct {
	Enabled        bool                 `yaml:"enabled" json:"enabled"`
	URL            string               `yaml:"url" json:"url"`
	Dataset        string               `yaml:"dataset" json:"dataset"`
	PricesDataset  string               `yaml:"prices-dataset" json:"prices-dataset"`
	TokenEnv       string               `yaml:"token-env" json:"token-env"`
	ExportProject  bool                 `yaml:"export-project" json:"export-project"`
	QuotaSnapshots QuotaSnapshotsConfig `yaml:"quota-snapshots" json:"quota-snapshots"`
}

type QuotaSnapshotsConfig struct {
	Enabled         bool              `yaml:"enabled" json:"enabled"`
	IntervalSeconds int               `yaml:"interval-seconds" json:"interval-seconds"`
	PlanPricesUSD   map[string]string `yaml:"plan-prices-usd" json:"plan-prices-usd"`
}

func (c QuotaSnapshotsConfig) QuotaSnapshotInterval() time.Duration {
	if c.IntervalSeconds <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(c.IntervalSeconds) * time.Second
}

var analyticsDecimalPattern = regexp.MustCompile(`^(0|[1-9]\d{0,11})(\.\d{1,12})?$`)
var analyticsDatasetPattern = regexp.MustCompile(`^[a-z0-9_-]+/[a-z0-9_]+$`)

type AccountingOutboxConfig struct {
	Analytics       AnalyticsExporterConfig `yaml:"analytics" json:"analytics"`
	Pricing         PricingConfig           `yaml:"pricing" json:"pricing"`
	Enabled         bool                    `yaml:"enabled" json:"enabled"`
	DataPath        string                  `yaml:"data-path" json:"data-path"`
	QueueCapacity   int                     `yaml:"queue-capacity" json:"queue-capacity"`
	MaxDiskMB       int                     `yaml:"max-disk-mb" json:"max-disk-mb"`
	MaxEvents       int                     `yaml:"max-events" json:"max-events"`
	RetentionHours  int                     `yaml:"retention-hours" json:"retention-hours"`
	ShutdownSeconds int                     `yaml:"shutdown-seconds" json:"shutdown-seconds"`
	ProjectPaths    map[string]string       `yaml:"project-paths" json:"project-paths"`
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
	if c.Analytics.Enabled {
		u, err := url.Parse(c.Analytics.URL)
		if !c.Enabled || err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !analyticsDatasetPattern.MatchString(c.Analytics.Dataset) {
			return fmt.Errorf("Analytics exporter requires an enabled outbox, HTTP(S) URL without credentials or query, and an owner/name dataset")
		}
		if c.Analytics.PricesDataset != "" && (!analyticsDatasetPattern.MatchString(c.Analytics.PricesDataset) || c.Analytics.PricesDataset == c.Analytics.Dataset) {
			return fmt.Errorf("Analytics prices-dataset must be a different owner/name dataset")
		}
	}
	if c.Analytics.QuotaSnapshots.Enabled && !c.Analytics.Enabled {
		return fmt.Errorf("Analytics quota-snapshots requires the Analytics exporter to be enabled")
	}
	if c.Analytics.QuotaSnapshots.IntervalSeconds < 0 || c.Analytics.QuotaSnapshots.IntervalSeconds > 86400 {
		return fmt.Errorf("invalid Analytics quota-snapshots interval-seconds")
	}
	for plan, price := range c.Analytics.QuotaSnapshots.PlanPricesUSD {
		if strings.TrimSpace(plan) == "" || !analyticsDecimalPattern.MatchString(price) {
			return fmt.Errorf("invalid Analytics quota-snapshots plan-prices-usd entry %q", plan)
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
