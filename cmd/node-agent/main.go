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
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	var (
		showVersion   = flag.Bool("version", false, "Print the version and exit")
		proxyAddr     = flag.String("proxy-addr", "127.0.0.1:50051", "LLM-proxy gRPC address (host:port)")
		useTLS        = flag.Bool("tls", false, "Connect to the proxy over TLS (system CA roots)")
		expose        = flag.String("expose", nodeagent.ExposeNone, "How the proxy reaches this node: "+nodeagent.ExposeCloudflared+" (built-in gateway + quick tunnel) or "+nodeagent.ExposeNone+" (you supply --endpoint)")
		cloudflared   = flag.String("cloudflared-path", "", "cloudflared binary for --expose=cloudflared (default: from PATH)")
		nodeID        = flag.String("node-id", "", "Unique Node ID (defaults to hostname)")
		nodeName      = flag.String("node-name", "", "Human-readable node name")
		engineURL     = flag.String("engine-url", "", "Local engine URL the agent polls (default "+nodeagent.DefaultEngineURL+", or --endpoint if only that is set)")
		endpoint      = flag.String("endpoint", "", "Address the proxy dials to reach the engine (default: --engine-url)")
		engine        = flag.String("engine", "ollama", "Inference engine (ollama, vllm, tgi, llamacpp)")
		keyFile       = flag.String("node-key-file", "", "Path to file containing node authentication key")
		baseWeight    = flag.Float64("base-weight", 100.0, "Base routing weight")
		maxConcurrent = flag.Uint("max-concurrent", 16, "Maximum concurrent requests")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	nodeKey := os.Getenv("NODE_KEY")
	if nodeKey == "" && *keyFile != "" {
		data, err := os.ReadFile(*keyFile)
		if err != nil {
			log.Fatal().Err(err).Str("file", *keyFile).Msg("failed to read node-key-file")
		}
		nodeKey = strings.TrimSpace(string(data))
	}
	if nodeKey == "" {
		log.Fatal().Msg("NODE_KEY environment variable or --node-key-file is required")
	}

	id := *nodeID
	if id == "" {
		h, err := os.Hostname()
		if err != nil {
			log.Fatal().Err(err).Msg("failed to determine hostname for default node-id")
		}
		id = h
	}

	name := *nodeName
	if name == "" {
		name = id
	}

	pollURL, reportURL := nodeagent.ResolveURLs(*engineURL, *endpoint)

	cfg := nodeagent.AgentConfig{
		ProxyAddr:      *proxyAddr,
		TLS:            *useTLS,
		NodeKey:        nodeKey,
		NodeID:         id,
		NodeName:       name,
		EngineURL:      pollURL,
		Endpoint:       reportURL,
		Engine:         *engine,
		NodeAuthToken:  os.Getenv("NODE_AUTH_TOKEN"),
		EngineToken:    os.Getenv("NODE_AUTH_TOKEN"),
		Expose:         *expose,
		CloudflaredBin: *cloudflared,
		BaseWeight:     *baseWeight,
		MaxConcurrent:  uint32(*maxConcurrent),
	}

	agent := nodeagent.NewAgent(cfg)
	ctx := context.Background()
	log.Info().Str("node_id", id).Str("engine", *engine).Str("engine_url", pollURL).Str("endpoint", reportURL).Msg("starting node-agent")

	if err := agent.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "node-agent error: %v\n", err)
		os.Exit(1)
	}
}
