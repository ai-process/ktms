package nodeagent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
)

type AgentConfig struct {
	ProxyAddr      string
	TLS            bool // dial ProxyAddr over TLS with system roots, e.g. a load balancer on :443
	NodeKey        string
	NodeID         string
	NodeName       string
	EngineURL      string // where the agent itself polls the engine; local, never reported
	Endpoint       string // address the proxy dials, as reported at registration
	Engine         string
	NodeAuthToken  string   // bearer the proxy presents to Endpoint; replaced by a generated key when exposing
	EngineToken    string   // bearer the engine itself requires, if any
	OnlyModels     []string // when set, report only these engine models
	JudgeModels    []string // models to tag with capability "judge"
	Expose         string   // ExposeNone or ExposeCloudflared
	CloudflaredBin string
	BaseWeight     float64
	MaxConcurrent  uint32
}

type Agent struct {
	cfg       AgentConfig
	client    pb.NodeRegistryServiceClient
	inspector EngineInspector
	drainChan chan struct{}
	stopChan  chan struct{}
	draining  bool
	endpoint  string // currently reported address; changes when the tunnel restarts
	mu        sync.Mutex
	probe     func(ctx context.Context, url string) error

	retryInitial, retryMax time.Duration // pause between refused registrations
}

// ResolveURLs returns the engine URL to poll and the endpoint to report.
// With only endpoint set, polling uses it too, as before --engine-url existed.
func ResolveURLs(engineURL, endpoint string) (poll, report string) {
	poll = engineURL
	if poll == "" {
		poll = endpoint
	}
	if poll == "" {
		poll = DefaultEngineURL
	}
	report = endpoint
	if report == "" {
		report = poll
	}
	return poll, report
}

// DefaultEngineURL is Ollama's default listen address.
const DefaultEngineURL = "http://127.0.0.1:11434"

func NewAgent(cfg AgentConfig) *Agent {
	return &Agent{
		cfg:       cfg,
		inspector: GetInspector(cfg.Engine),
		drainChan: make(chan struct{}, 1),
		stopChan:  make(chan struct{}),
		endpoint:  cfg.Endpoint,
		probe:     httpProbe,

		retryInitial: retryInitial,
		retryMax:     retryMax,
	}
}

// dialCredentials picks plaintext or system-root TLS for the proxy connection.
func (a *Agent) dialCredentials() credentials.TransportCredentials {
	if a.cfg.TLS {
		return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	return insecure.NewCredentials()
}

// snapshot reads what the proxy tracks per node: engine models and GPU state.
func (a *Agent) snapshot(ctx context.Context) ([]*pb.NodeModelSpec, *pb.NodeResources) {
	models, err := a.inspector.Inspect(ctx, a.cfg.EngineURL, a.cfg.EngineToken)
	if err != nil {
		log.Warn().Err(err).Msg("engine inspection failed, reporting no models")
	}
	models = FilterModels(models, a.cfg.OnlyModels)
	if len(a.cfg.JudgeModels) > 0 {
		models = TagJudgeModels(models, a.cfg.JudgeModels)
	}
	res := &pb.NodeResources{BaseWeight: a.cfg.BaseWeight, MaxConcurrent: a.cfg.MaxConcurrent}
	if gpu, _ := ReadNvidiaSMI(ctx); gpu != nil {
		res.VramTotalMb = gpu.TotalVRAMMB
		res.VramFreeMb = gpu.FreeVRAMMB
		res.GpuUtilPercent = gpu.UtilPercent
		res.GpuTempC = gpu.TempC
		res.GpuModel = gpu.ModelName
	}
	return models, res
}

// FilterModels keeps only the allowed models; an empty allow list keeps all.
// An engine shared with other tools would otherwise advertise models this node must not serve.
func FilterModels(models []*pb.NodeModelSpec, allow []string) []*pb.NodeModelSpec {
	if len(allow) == 0 {
		return models
	}
	var kept []*pb.NodeModelSpec
	for _, m := range models {
		for _, name := range allow {
			if sameModel(m.GetModelId(), name) || sameModel(m.GetRemoteModelName(), name) {
				kept = append(kept, m)
				break
			}
		}
	}
	return kept
}

// TagJudgeModels ensures models in judge list declare the "judge" capability.
func TagJudgeModels(models []*pb.NodeModelSpec, judgeModels []string) []*pb.NodeModelSpec {
	judgeSet := make(map[string]bool, len(judgeModels))
	for _, jm := range judgeModels {
		judgeSet[jm] = true
	}
	for _, m := range models {
		if judgeSet[m.GetModelId()] || judgeSet[m.GetRemoteModelName()] {
			hasJudge := false
			for _, c := range m.Capabilities {
				if c == "judge" {
					hasJudge = true
					break
				}
			}
			if !hasJudge {
				m.Capabilities = append(m.Capabilities, "judge")
			}
		}
	}
	return models
}

func (a *Agent) register(ctx context.Context, models []*pb.NodeModelSpec, res *pb.NodeResources) (*pb.RegisterNodeResponse, error) {
	a.mu.Lock()
	endpoint, token := a.endpoint, a.cfg.NodeAuthToken
	a.mu.Unlock()
	return a.client.RegisterNode(ctx, &pb.RegisterNodeRequest{
		NodeId:        a.cfg.NodeID,
		Name:          a.cfg.NodeName,
		Endpoint:      endpoint,
		Engine:        a.cfg.Engine,
		NodeAuthToken: token,
		Resources:     res,
		Models:        models,
	})
}

func (a *Agent) currentEndpoint() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.endpoint
}

func (a *Agent) setEndpoint(u string) {
	a.mu.Lock()
	a.endpoint = u
	a.mu.Unlock()
}

func (a *Agent) Run(ctx context.Context) error {
	conn, err := grpclib.DialContext(ctx, a.cfg.ProxyAddr, grpclib.WithTransportCredentials(a.dialCredentials()))
	if err != nil {
		return fmt.Errorf("connect to proxy %s: %w", a.cfg.ProxyAddr, err)
	}
	defer conn.Close()

	a.client = pb.NewNodeRegistryServiceClient(conn)

	// Context carrying Bearer token
	authCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+a.cfg.NodeKey)

	// Handle SIGTERM/SIGINT for graceful drain
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case sig := <-sigCh:
			log.Info().Str("signal", sig.String()).Msg("received shutdown signal, initiating graceful drain")
			a.mu.Lock()
			a.draining = true
			a.mu.Unlock()
			select {
			case a.drainChan <- struct{}{}:
			default:
			}
		case <-a.stopChan:
			return
		}
	}()

	// Learn about a permanent refusal before any tunnel is started.
	if err := a.preflight(authCtx); err != nil {
		return a.stopOrFail(ctx, err)
	}

	// The gateway and tunnel must outlive drainAndExit: the proxy still sends requests while draining.
	expCtx, stopExposure := context.WithCancel(ctx)
	urls, stopped, err := a.startExposure(expCtx)
	defer func() { // cloudflared must be gone before the process exits, or it is orphaned
		stopExposure()
		if stopped != nil {
			select {
			case <-stopped:
			case <-time.After(10 * time.Second):
				log.Warn().Msg("tunnel did not stop in time")
			}
		}
	}()
	if err != nil {
		return err
	}
	if urls != nil {
		log.Info().Msg("waiting for the public tunnel address")
		select {
		case u := <-urls:
			a.setEndpoint(u)
		case <-time.After(2 * time.Minute):
			return errors.New("cloudflared produced no public address within 2 minutes; check that this machine can reach Cloudflare (outbound port 7844) or use --expose=none")
		case <-a.drainChan:
			log.Info().Msg("shutdown requested before the tunnel was ready")
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// 1. Initial Inspection & Registration
	regResp, err := a.registerUntilAccepted(authCtx, urls)
	if err != nil {
		return a.stopOrFail(ctx, err)
	}

	interval := 5 * time.Second
	if regResp.HeartbeatIntervalSeconds > 0 {
		interval = time.Duration(regResp.HeartbeatIntervalSeconds) * time.Second
	}

	log.Info().Str("node_id", a.cfg.NodeID).Str("endpoint", a.currentEndpoint()).Dur("interval", interval).Msg("node registered successfully")

	// 2. Heartbeat loop
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	needRegister := false
	var retryAt time.Time
	bo := a.newBackoff()
	reregister := func() {
		models, res := a.snapshot(authCtx)
		if _, err := a.register(authCtx, models, res); err != nil {
			needRegister = true
			delay := bo.next()
			retryAt = time.Now().Add(delay)
			log.Warn().Err(err).Dur("retry_in", delay).Msg("re-registration failed: " + explainRegistrationError(err))
			return
		}
		needRegister = false
		bo.reset()
		log.Info().Str("endpoint", a.currentEndpoint()).Msg("node re-registered")
	}

	for {
		select {
		case <-ctx.Done():
			return a.unregister(authCtx)
		case <-a.drainChan:
			return a.drainAndExit(authCtx)
		case u := <-urls: // nil when nothing is exposed
			log.Info().Str("endpoint", u).Msg("tunnel address changed")
			a.setEndpoint(u)
			reregister()
		case <-ticker.C:
			currModels, currRes := a.snapshot(authCtx)

			a.mu.Lock()
			draining := a.draining
			a.mu.Unlock()

			hbResp, err := a.client.Heartbeat(authCtx, &pb.HeartbeatRequest{
				NodeId:    a.cfg.NodeID,
				Draining:  draining,
				Resources: currRes,
				Models:    currModels,
			})
			if err != nil {
				log.Warn().Err(err).Msg("heartbeat failed")
				continue
			}

			if hbResp.ReRegisterRequired {
				log.Info().Msg("proxy requested re-registration")
				needRegister = true
			}
			if needRegister && !time.Now().Before(retryAt) {
				reregister()
			}

			if hbResp.DrainRequested && !draining {
				log.Info().Msg("proxy requested drain")
				a.mu.Lock()
				a.draining = true
				a.mu.Unlock()
				select {
				case a.drainChan <- struct{}{}:
				default:
				}
			}
		}
	}
}

// stopOrFail ends Run after a wait was cut short: a shutdown request is a clean exit.
func (a *Agent) stopOrFail(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errStopped) {
		log.Info().Msg("shutdown requested before the node was registered")
		return nil
	}
	return err
}

func (a *Agent) drainAndExit(ctx context.Context) error {
	log.Info().Msg("waiting for active proxy requests to finish before stopping")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		hbResp, err := a.client.Heartbeat(ctx, &pb.HeartbeatRequest{
			NodeId:   a.cfg.NodeID,
			Draining: true,
		})
		if err == nil {
			if hbResp.ActiveProxyRequests <= 0 {
				log.Info().Msg("active requests reached 0, exiting cleanly")
				return a.unregister(ctx)
			}
			log.Info().Int32("active_requests", hbResp.ActiveProxyRequests).Msg("still draining...")
		}
		select {
		case <-ctx.Done():
			return a.unregister(ctx)
		case <-ticker.C:
		}
	}
}

func (a *Agent) unregister(ctx context.Context) error {
	close(a.stopChan)
	log.Info().Str("node_id", a.cfg.NodeID).Msg("unregistering node from proxy")
	_, err := a.client.UnregisterNode(ctx, &pb.UnregisterNodeRequest{
		NodeId: a.cfg.NodeID,
		Reason: "agent shutdown",
	})
	return err
}
