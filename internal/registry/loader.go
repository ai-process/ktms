package registry

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/ai-process/llm-proxy/internal/crypto"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/panicsafe"
	"github.com/ai-process/llm-proxy/internal/proxydb"
)

// Loader keeps the active Snapshot fresh: it polls config_state and rebuilds
// on version change. A failed rebuild keeps the last good snapshot serving.
type Loader struct {
	db       *proxydb.DB
	keyring  *crypto.Keyring
	tracker  llm.UsageTracker
	nodePool NodePoolResolver
	current  atomic.Pointer[Snapshot]
	// refreshCh coalesces manual refresh requests from admin mutations.
	refreshCh chan struct{}
}

func NewLoader(db *proxydb.DB, keyring *crypto.Keyring, tracker llm.UsageTracker) *Loader {
	l := &Loader{db: db, keyring: keyring, tracker: tracker, refreshCh: make(chan struct{}, 1)}
	l.current.Store(&Snapshot{Models: map[string]*proxydb.Model{}})
	return l
}

func (l *Loader) SetNodePool(np NodePoolResolver) {
	l.nodePool = np
	if cur := l.current.Load(); cur != nil {
		cur.SetNodePool(np)
	}
}

// Snapshot returns the active config view; never nil.
func (l *Loader) Snapshot() *Snapshot { return l.current.Load() }

// Refresh asks the poll loop to rebuild now (after an admin mutation in this
// replica). Non-blocking; a pending request is enough.
func (l *Loader) Refresh() {
	select {
	case l.refreshCh <- struct{}{}:
	default:
	}
}

// Load rebuilds the snapshot unconditionally. Called once at startup (an
// error is fatal there) and by the poll loop.
func (l *Loader) Load(ctx context.Context) error {
	version, err := l.db.ConfigVersion(ctx)
	if err != nil {
		return fmt.Errorf("config version: %w", err)
	}
	snap, err := l.build(ctx, version)
	if err != nil {
		return err
	}
	l.current.Store(snap)
	log.Info().Int64("version", version).Int("models", len(snap.Models)).
		Int("rules", len(snap.Rules)).Msg("routing config loaded")
	return nil
}

// Start polls for config changes every interval until ctx is cancelled.
func (l *Loader) Start(ctx context.Context, interval time.Duration) {
	go func() {
		defer panicsafe.Guard("registry loader")
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				l.reloadIfChanged(ctx)
			case <-l.refreshCh:
				if err := l.Load(ctx); err != nil {
					log.Error().Err(err).Msg("config refresh failed; keeping last good snapshot")
				}
			}
		}
	}()
}

func (l *Loader) reloadIfChanged(ctx context.Context) {
	version, err := l.db.ConfigVersion(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("config version poll failed")
		return
	}
	if version == l.current.Load().Version {
		return
	}
	if err := l.Load(ctx); err != nil {
		// Can only happen via manual SQL — admin RPCs validate before commit.
		log.Error().Err(err).Msg("config reload failed; keeping last good snapshot")
	}
}

func (l *Loader) build(ctx context.Context, version int64) (*Snapshot, error) {
	models, err := l.db.LoadModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("load models: %w", err)
	}
	rules, err := l.db.LoadRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("load rules: %w", err)
	}
	if err := Validate(models, rules); err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}
	vendorKeys, err := l.db.LoadVendorKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("load vendor keys: %w", err)
	}
	userRateLimits, err := l.db.LoadUserRateLimits(ctx)
	if err != nil {
		return nil, fmt.Errorf("load user rate limits: %w", err)
	}
	spendLimits, err := l.db.LoadSpendLimits(ctx)
	if err != nil {
		return nil, fmt.Errorf("load spend limits: %w", err)
	}

	snap := &Snapshot{
		Version:        version,
		Models:         map[string]*proxydb.Model{},
		vendorKeys:     map[string]map[string]string{},
		UserRateLimits: map[string]map[string]*proxydb.UserRateLimit{},
		SpendLimits:    map[string]map[string]int64{},
		tracker:        l.tracker,
		nodePool:       l.nodePool,
	}
	for _, u := range userRateLimits {
		if snap.UserRateLimits[u.UserID] == nil {
			snap.UserRateLimits[u.UserID] = map[string]*proxydb.UserRateLimit{}
		}
		snap.UserRateLimits[u.UserID][u.ModelID] = u
	}
	for _, sl := range spendLimits {
		k := spendKey(sl.SubjectType, sl.SubjectID)
		if snap.SpendLimits[k] == nil {
			snap.SpendLimits[k] = map[string]int64{}
		}
		snap.SpendLimits[k][sl.Period] = sl.LimitNanoUSD
	}
	for _, m := range models {
		if m.Enabled {
			snap.Models[m.ID] = m
		}
	}
	for _, r := range rules { // already sorted priority desc by the query
		if r.Enabled {
			snap.Rules = append(snap.Rules, r)
		}
	}
	for _, vk := range vendorKeys {
		plain, err := l.keyring.Decrypt(vk.KeyCiphertext, int(vk.KeyVersion))
		if err != nil {
			// Never log key material; id and version identify the row safely.
			return nil, fmt.Errorf("decrypt vendor key %s (version %d): %w", vk.ID, vk.KeyVersion, err)
		}
		if snap.vendorKeys[vk.APIKeyID] == nil {
			snap.vendorKeys[vk.APIKeyID] = map[string]string{}
		}
		snap.vendorKeys[vk.APIKeyID][vk.Vendor] = plain
	}
	return snap, nil
}
