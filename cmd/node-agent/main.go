package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/llm-proxy/internal/nodeagent"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	// Service managers redirect stderr to a file; colors there are just escape noise.
	fi, _ := os.Stderr.Stat()
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, NoColor: fi == nil || fi.Mode()&os.ModeCharDevice == 0})

	args := os.Args[1:]
	if len(args) > 0 && args[0] == "doctor" {
		os.Exit(runDoctor(args[1:]))
	}
	os.Exit(runAgent(args))
}

// settings are the flags the agent and the doctor share.
type settings struct {
	fs            *flag.FlagSet
	showVersion   *bool
	proxyAddr     *string
	useTLS        *bool
	expose        *string
	cloudflared   *string
	nodeID        *string
	nodeName      *string
	engineURL     *string
	endpoint      *string
	engine        *string
	keyFile       *string
	baseWeight    *float64
	maxConcurrent *uint
	onlyModels    stringList
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func newSettings(name string) *settings {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	s := &settings{
		fs:            fs,
		showVersion:   fs.Bool("version", false, "Print the version and exit"),
		proxyAddr:     fs.String("proxy-addr", "127.0.0.1:50051", "LLM-proxy gRPC address (host:port)"),
		useTLS:        fs.Bool("tls", false, "Connect to the proxy over TLS (system CA roots)"),
		expose:        fs.String("expose", nodeagent.ExposeNone, "How the proxy reaches this node: "+nodeagent.ExposeCloudflared+" (built-in gateway + quick tunnel) or "+nodeagent.ExposeNone+" (you supply --endpoint)"),
		cloudflared:   fs.String("cloudflared-path", "", "cloudflared binary for --expose=cloudflared (default: from PATH)"),
		nodeID:        fs.String("node-id", "", "Unique Node ID (defaults to hostname)"),
		nodeName:      fs.String("node-name", "", "Human-readable node name"),
		engineURL:     fs.String("engine-url", "", "Local engine URL the agent polls (default "+nodeagent.DefaultEngineURL+", or --endpoint if only that is set)"),
		endpoint:      fs.String("endpoint", "", "Address the proxy dials to reach the engine (default: --engine-url)"),
		engine:        fs.String("engine", "ollama", "Inference engine (ollama, vllm, tgi, llamacpp)"),
		keyFile:       fs.String("node-key-file", "", "Path to file containing node authentication key"),
		baseWeight:    fs.Float64("base-weight", 100.0, "Base routing weight"),
		maxConcurrent: fs.Uint("max-concurrent", 16, "Maximum concurrent requests"),
	}
	fs.Var(&s.onlyModels, "only-model", "Report only this engine model to the proxy; repeatable (default: every model the engine has)")
	return s
}

// config resolves the flags and environment into an agent configuration.
func (s *settings) config() (nodeagent.AgentConfig, error) {
	nodeKey := os.Getenv("NODE_KEY")
	if nodeKey == "" && *s.keyFile != "" {
		data, err := os.ReadFile(*s.keyFile)
		if err != nil {
			return nodeagent.AgentConfig{}, fmt.Errorf("cannot read --node-key-file %s: %w", *s.keyFile, err)
		}
		nodeKey = strings.TrimSpace(string(data))
	}
	if nodeKey == "" {
		return nodeagent.AgentConfig{}, fmt.Errorf("NODE_KEY environment variable or --node-key-file is required")
	}

	id := *s.nodeID
	if id == "" {
		h, err := os.Hostname()
		if err != nil {
			return nodeagent.AgentConfig{}, fmt.Errorf("cannot determine hostname for the default node-id: %w", err)
		}
		id = h
	}
	name := *s.nodeName
	if name == "" {
		name = id
	}
	pollURL, reportURL := nodeagent.ResolveURLs(*s.engineURL, *s.endpoint)

	return nodeagent.AgentConfig{
		ProxyAddr:      *s.proxyAddr,
		TLS:            *s.useTLS,
		NodeKey:        nodeKey,
		NodeID:         id,
		NodeName:       name,
		EngineURL:      pollURL,
		Endpoint:       reportURL,
		Engine:         *s.engine,
		NodeAuthToken:  os.Getenv("NODE_AUTH_TOKEN"),
		EngineToken:    os.Getenv("NODE_AUTH_TOKEN"),
		OnlyModels:     s.onlyModels,
		Expose:         *s.expose,
		CloudflaredBin: *s.cloudflared,
		BaseWeight:     *s.baseWeight,
		MaxConcurrent:  uint32(*s.maxConcurrent),
	}, nil
}

func runAgent(args []string) int {
	s := newSettings(os.Args[0])
	_ = s.fs.Parse(args) // ExitOnError
	if *s.showVersion {
		fmt.Println(version)
		return 0
	}
	cfg, err := s.config()
	if err != nil {
		log.Error().Err(err).Msg("invalid configuration")
		return 1
	}

	log.Info().Str("node_id", cfg.NodeID).Str("engine", cfg.Engine).Str("engine_url", cfg.EngineURL).Str("endpoint", cfg.Endpoint).Msg("starting node-agent")
	if err := nodeagent.NewAgent(cfg).Run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "node-agent error: %v\n", err)
		return 1
	}
	return 0
}

// runDoctor explains in plain language why a node is not serving. Exit 1 if it finds a problem.
func runDoctor(args []string) int {
	s := newSettings(os.Args[0] + " doctor")
	model := s.fs.String("model", "", "Model this node should serve; checks it is installed and loaded")
	_ = s.fs.Parse(args) // ExitOnError
	cfg, err := s.config()
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor: %v\n", err)
		return 1
	}
	fmt.Printf("Checking node %s against %s (engine %s at %s)\n\n", cfg.NodeID, cfg.ProxyAddr, cfg.Engine, cfg.EngineURL)
	d := &nodeagent.Doctor{Cfg: cfg, Model: *model}
	if nodeagent.Render(os.Stdout, d.Run(context.Background())) > 0 {
		return 1
	}
	return 0
}
