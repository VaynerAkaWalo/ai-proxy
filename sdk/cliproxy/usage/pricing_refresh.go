package usage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	maxCatalogBytes     = 64 << 20
	catalogRetryBackoff = 15 * time.Minute
)

// refreshCatalogLoop downloads the catalog immediately and then every configured interval.
// A failed refresh keeps the current price book and retries sooner than the normal interval.
func (o *Outbox) refreshCatalogLoop(ctx context.Context) {
	interval := time.Duration(o.cfg.Pricing.LiteLLMCatalogRefreshHours) * time.Hour
	for {
		delay := interval
		if err := o.refreshCatalog(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warnf("price catalog refresh failed, keeping current prices: %v", err)
			delay = min(catalogRetryBackoff, interval)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (o *Outbox) refreshCatalog(ctx context.Context) error {
	raw, err := downloadCatalog(ctx, o.cfg.Pricing.LiteLLMCatalogURL)
	if err != nil {
		return err
	}
	catalog, err := importLiteLLM(raw)
	if err != nil {
		return err
	}
	book := newCatalogPriceBook(o.cfg.Pricing, catalog)
	if book == nil {
		return fmt.Errorf("downloaded catalog does not form a valid price book")
	}
	writeCatalogCache(filepath.Join(o.cfg.DataPath, catalogCacheFile), raw)
	o.prices.Store(book)
	log.Infof("price catalog refreshed: %d rates from %s", len(catalog), o.cfg.Pricing.LiteLLMCatalogURL)
	return nil
}

func downloadCatalog(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog download returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxCatalogBytes {
		return nil, fmt.Errorf("catalog exceeds %d bytes", maxCatalogBytes)
	}
	return raw, nil
}
