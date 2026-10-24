package nodeagent

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

const (
	defaultStatsEvery = time.Minute
	statsPageLimit    = 1000
	statsMaxPages     = 20 // per tick, so a long catch-up never stalls heartbeats
	statsRetryLater   = time.Hour
	statsPullTimeout  = 10 * time.Second
)

// statsState is the agent's usage-log bookkeeping; only Run's goroutine touches it.
type statsState struct {
	store     *StatsStore
	cursor    string
	retryAt   time.Time
	failing   bool // the last pull failed; repeats stay quiet
	maintDay  string
	beat      Sample // fields from the last heartbeat; At is when it succeeded
	beatEvery time.Duration
}

// openStats prepares the local log; a node that cannot keep one still serves.
func (a *Agent) openStats() {
	if a.cfg.StatsDir == "" {
		return
	}
	st, err := OpenStats(a.cfg.StatsDir, true)
	if err == nil {
		a.stats.cursor, err = st.Cursor()
	}
	if err != nil {
		log.Error().Err(err).Str("dir", a.cfg.StatsDir).Msg("cannot keep the usage log; serving without it")
		return
	}
	a.stats.store = st
}

// noteBeat keeps what the next minute's sample reports.
func (a *Agent) noteBeat(res *pb.NodeResources, models []*pb.NodeModelSpec, running, waiting uint32, proxyActive int32) {
	s := Sample{At: a.now(), ProxyActive: proxyActive, EngineRunning: running, EngineWaiting: waiting}
	if res != nil {
		s.GPUUtil, s.GPUTempC = res.GetGpuUtilPercent(), res.GetGpuTempC()
		s.VRAMFreeMB, s.VRAMTotalMB = res.GetVramFreeMb(), res.GetVramTotalMb()
	}
	for _, m := range models {
		if m.GetIsWarm() {
			s.Warm = append(s.Warm, m.GetModelId())
		}
	}
	a.stats.beat = s
}

// statsTick pulls new records, writes a sample and tidies old days.
func (a *Agent) statsTick(ctx context.Context) {
	st := a.stats.store
	if st == nil {
		return
	}
	a.pullUsage(ctx)
	if err := st.AppendSample(a.sample()); err != nil {
		log.Warn().Err(err).Str("dir", st.Dir()).Msg("cannot write a usage sample")
	}
	if day := st.day(a.now()); day != a.stats.maintDay {
		if err := st.Maintain(a.now(), a.cfg.StatsRetention); err != nil {
			log.Warn().Err(err).Str("dir", st.Dir()).Msg("usage log upkeep failed")
		}
		a.stats.maintDay = day
	}
}

func (a *Agent) sample() Sample {
	now := a.now()
	s := a.stats.beat
	s.At, s.Level = now, a.pause.level
	a.mu.Lock()
	draining := a.draining
	a.mu.Unlock()
	switch {
	case a.stats.beat.At.IsZero() || now.Sub(a.stats.beat.At) > 3*a.stats.beatEvery:
		s = Sample{At: now, Level: a.pause.level, State: SampleStateOffline}
	case draining:
		s.State = SampleStateDraining
	case a.pause.level == 0:
		s.State = SampleStatePaused
	default:
		s.State = SampleStateServing
	}
	return s
}

// pullUsage copies the proxy's new records for this node to disk. The cursor
// moves only after the records are synced.
func (a *Agent) pullUsage(ctx context.Context) {
	st := &a.stats
	if a.now().Before(st.retryAt) {
		return
	}
	for range statsMaxPages {
		pctx, cancel := context.WithTimeout(ctx, statsPullTimeout)
		resp, err := a.client.PullNodeUsage(pctx, &pb.PullNodeUsageRequest{NodeId: a.cfg.NodeID, AfterId: st.cursor, Limit: statsPageLimit})
		cancel()
		if err != nil {
			a.pullFailed(err)
			return
		}
		if st.failing {
			log.Info().Msg("usage log pulls work again")
			st.failing = false
		}
		if gu := resp.GetGapUntil(); gu != nil {
			g := Gap{From: streamIDTime(st.cursor), Until: gu.AsTime()}
			log.Warn().Time("from", g.From).Time("until", g.Until).Msg("the proxy no longer had part of this node's usage log; that stretch is missing")
			if err := st.store.AppendGap(g); err != nil {
				log.Warn().Err(err).Msg("cannot record a usage log gap")
				return
			}
		}
		recs := make([]RequestRecord, 0, len(resp.GetRecords()))
		for _, r := range resp.GetRecords() {
			recs = append(recs, requestRecordFrom(r))
		}
		if err := st.store.AppendRequests(recs); err != nil {
			log.Warn().Err(err).Str("dir", st.store.Dir()).Msg("cannot write the usage log; will pull again")
			return
		}
		if next := resp.GetNextId(); next != st.cursor {
			if err := st.store.SetCursor(next); err != nil {
				log.Warn().Err(err).Msg("cannot save the usage log position; records may repeat")
				return
			}
			st.cursor = next
		} else if err := st.store.Touch(a.now()); err != nil {
			log.Warn().Err(err).Msg("cannot mark the usage log pull")
		}
		if !resp.GetMore() {
			return
		}
	}
}

func (a *Agent) pullFailed(err error) {
	st := &a.stats
	switch status.Code(err) {
	case codes.Unimplemented:
		st.retryAt = a.now().Add(statsRetryLater)
		if !st.failing {
			log.Warn().Msg("the proxy predates the usage log; this node keeps only its own samples until it is upgraded")
		}
	case codes.FailedPrecondition:
		st.retryAt = a.now().Add(statsRetryLater)
		if !st.failing {
			log.Warn().Err(err).Msg("the proxy keeps no usage log (NODE_USAGE_RETENTION_HOURS=0); this node keeps only its own samples")
		}
	default:
		if !st.failing {
			log.Warn().Err(err).Msg("usage log pull failed; retrying every minute")
		}
	}
	st.failing = true
}

func requestRecordFrom(r *pb.NodeUsageRecord) RequestRecord {
	return RequestRecord{
		ID:              r.GetId(),
		RequestID:       r.GetRequestId(),
		StartedAt:       r.GetStartedAt().AsTime(),
		DurationMs:      r.GetDurationMs(),
		TTFTMs:          r.GetTtftMs(),
		Model:           r.GetModelId(),
		Kind:            r.GetKind(),
		InputTokens:     r.GetInputTokens(),
		OutputTokens:    r.GetOutputTokens(),
		CacheReadTokens: r.GetCacheReadTokens(),
		Outcome:         r.GetOutcome(),
		ErrorCode:       r.GetErrorCode(),
		Level:           r.GetCapacityPercent(),
		CooldownMs:      r.GetCooldownMs(),
	}
}

// streamIDTime is when a proxy log id was written ("1729760000000-0"); zero for "".
func streamIDTime(id string) time.Time {
	ms := int64(0)
	for _, c := range id {
		if c < '0' || c > '9' {
			break
		}
		ms = ms*10 + int64(c-'0')
	}
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
