package cliproxy

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

type accountingExporter interface {
	Start()
	Stop(context.Context) error
	Status() string
}

func (s *Service) startAccountingOutbox() error {
	s.accountingMu.Lock()
	defer s.accountingMu.Unlock()
	if s.cfg == nil || !s.cfg.AccountingOutbox.Enabled || s.accountingOutbox != nil {
		return nil
	}
	cfg := s.cfg.AccountingOutbox.Defaults()
	if err := cfg.Validate(); err != nil {
		return err
	}
	outbox, err := usage.OpenOutbox(cfg)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"persistence_failures": 1, "accounting_unavailable": true}).Error("accounting disabled for this service run")
		outbox = usage.UnavailableOutbox()
	}
	detach, err := usage.DefaultManager().AttachOutbox(outbox)
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.ShutdownSeconds)*time.Second)
		defer cancel()
		if errClose := outbox.Close(ctx); errClose != nil {
			log.WithError(errClose).Warn("close unattached accounting store")
		}
		return err
	}
	s.accountingOutbox, s.accountingDetach, s.accountingShutdownSeconds = outbox, detach, cfg.ShutdownSeconds
	if cfg.Analytics.Enabled {
		s.accountingExporter = usage.NewAnalyticsExporter(outbox, cfg.Analytics)
	} else {
		s.accountingExporter = noopAccountingExporter{}
	}
	s.accountingExporter.Start()
	if cfg.Analytics.Enabled && cfg.Analytics.QuotaSnapshots.Enabled && s.coreManager != nil {
		s.quotaSampler = newQuotaSnapshotSampler(outbox, s.coreManager, cfg.Analytics.QuotaSnapshots, s.currentConfig)
		s.quotaSampler.Start()
	}
	return nil
}

func (s *Service) stopAccountingOutbox(ctx context.Context) {
	s.accountingMu.Lock()
	defer s.accountingMu.Unlock()
	if s.accountingOutbox == nil {
		return
	}
	s.accountingDetach()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.accountingShutdownSeconds)*time.Second)
	defer cancel()
	if s.quotaSampler != nil {
		s.quotaSampler.Stop(ctx)
		s.quotaSampler = nil
	}
	if s.accountingExporter != nil {
		if err := s.accountingExporter.Stop(ctx); err != nil {
			log.WithError(err).Warn("accounting exporter shutdown incomplete")
		}
	}
	if err := s.accountingOutbox.Close(ctx); err != nil {
		log.WithError(err).Warn("accounting shutdown incomplete, queued events are not durable")
	}
	stats := s.accountingOutbox.Counters()
	log.WithFields(log.Fields{"dropped_events": stats.DroppedEvents, "persistence_failures": stats.PersistenceFailures, "queue_depth": stats.QueueDepth}).Info("accounting stopped")
}

func (s *Service) AccountingOutboxStats() (usage.OutboxStats, error) {
	s.accountingMu.Lock()
	outbox := s.accountingOutbox
	s.accountingMu.Unlock()
	if outbox == nil {
		return usage.OutboxStats{}, nil
	}
	return outbox.Stats()
}

func (s *Service) AccountingExporterStatus() string {
	s.accountingMu.Lock()
	defer s.accountingMu.Unlock()
	if s.accountingExporter == nil {
		return "disabled"
	}
	return s.accountingExporter.Status()
}

type noopAccountingExporter struct{}

func (noopAccountingExporter) Start() {}

func (noopAccountingExporter) Stop(context.Context) error { return nil }

func (noopAccountingExporter) Status() string { return "disabled" }
