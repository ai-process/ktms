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
		proxyAddr     = flag.String("proxy-addr", "127.0.0.1:50051", "LLM-proxy gRPC address (host:port)")
		useTLS        = flag.Bool("tls", false, "Connect to the proxy over TLS (system CA roots)")
		nodeID        = flag.String("node-id", "", "Unique Node ID (defaults to hostname)")
		nodeName      = flag.String("node-name", "", "Human-readable node name")
		endpoint      = flag.String("endpoint", "http://127.0.0.1:11434", "Inference engine HTTP endpoint")
		engine        = flag.String("engine", "ollama", "Inference engine (ollama, vllm, tgi, llamacpp)")
		keyFile       = flag.String("node-key-file", "", "Path to file containing node authentication key")
		baseWeight    = flag.Float64("base-weight", 100.0, "Base routing weight")
		maxConcurrent = flag.Uint("max-concurrent", 16, "Maximum concurrent requests")
	)
	flag.Parse()

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

	cfg := nodeagent.AgentConfig{
		ProxyAddr:     *proxyAddr,
		TLS:           *useTLS,
		NodeKey:       nodeKey,
		NodeID:        id,
		NodeName:      name,
		Endpoint:      *endpoint,
		Engine:        *engine,
		NodeAuthToken: os.Getenv("NODE_AUTH_TOKEN"),
		BaseWeight:    *baseWeight,
		MaxConcurrent: uint32(*maxConcurrent),
	}

	agent := nodeagent.NewAgent(cfg)
	ctx := context.Background()
	log.Info().Str("node_id", id).Str("engine", *engine).Str("endpoint", *endpoint).Msg("starting node-agent")

	if err := agent.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "node-agent error: %v\n", err)
		os.Exit(1)
	}
}
