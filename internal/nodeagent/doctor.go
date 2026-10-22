package nodeagent

import (
	"context"
	"crypto/x509"
	"debug/macho"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// Level ranks a finding. Only a problem makes the doctor exit non-zero.
type Level int

const (
	LevelOK Level = iota
	LevelWarn
	LevelProblem
)

// Finding is one line of the report, in plain language, with what to do next.
type Finding struct {
	Check   string
	Level   Level
	Message string
	Fix     string
}

// Doctor explains why a node is not (or is badly) serving. It sends one
// Heartbeat for a made-up node id, which the proxy answers without storing.
type Doctor struct {
	Cfg   AgentConfig
	Model string // the model this node should serve; empty = any

	// Seams for tests; zero values use the real thing.
	HTTP       *http.Client
	DialTCP    func(ctx context.Context, addr string) error
	UsesProxy  func(addr string) bool // true when HTTPS_PROXY or similar routes addr; gRPC honors it, a raw dial does not
	LookPath   func(name string) (string, error)
	RunCmd     func(ctx context.Context, name string, args ...string) error
	MachOArchs func(path string) ([]string, error)
	GOOS       string
	GOARCH     string
}

const probeNodeID = "doctor-probe"

func (d *Doctor) defaults() {
	if d.HTTP == nil {
		d.HTTP = &http.Client{Timeout: 5 * time.Second}
	}
	if d.DialTCP == nil {
		d.DialTCP = func(ctx context.Context, addr string) error {
			c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", addr)
			if err != nil {
				return err
			}
			return c.Close()
		}
	}
	if d.UsesProxy == nil {
		d.UsesProxy = func(addr string) bool {
			u, err := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
			return err == nil && u != nil
		}
	}
	if d.LookPath == nil {
		d.LookPath = exec.LookPath
	}
	if d.RunCmd == nil {
		d.RunCmd = func(ctx context.Context, name string, args ...string) error {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return exec.CommandContext(ctx, name, args...).Run()
		}
	}
	if d.MachOArchs == nil {
		d.MachOArchs = machoArchs
	}
	if d.GOOS == "" {
		d.GOOS = runtime.GOOS
	}
	if d.GOARCH == "" {
		d.GOARCH = runtime.GOARCH
	}
}

// Run executes every check. A failed check never stops the later ones.
func (d *Doctor) Run(ctx context.Context) []Finding {
	d.defaults()
	var out []Finding
	models, f, engineUp := d.checkEngine(ctx)
	out = append(out, f...)
	if engineUp { // without an engine, "no models" would only repeat the same fault
		out = append(out, d.checkModel(models)...)
		out = append(out, d.checkAcceleration(ctx)...)
	}
	proxy := d.checkProxy(ctx)
	out = append(out, proxy...)
	if engineUp && !hasProblem(proxy) { // a dead proxy or key would only repeat itself here
		out = append(out, d.checkRegistration(ctx, models)...)
	}
	if d.Cfg.Expose == ExposeCloudflared {
		out = append(out, d.checkTunnel(ctx)...)
	} else {
		out = append(out, d.checkEndpoint(ctx)...)
	}
	return out
}

// ---- engine ---------------------------------------------------------------

func (d *Doctor) engineProbeURL() string {
	base := engineBaseURL(d.Cfg.EngineURL)
	if strings.EqualFold(d.Cfg.Engine, "ollama") {
		return base + "/api/version"
	}
	return base + "/v1/models"
}

func (d *Doctor) checkEngine(ctx context.Context) (models []*pb.NodeModelSpec, findings []Finding, up bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.engineProbeURL(), nil)
	if err != nil {
		return nil, []Finding{{"Engine", LevelProblem, fmt.Sprintf("The engine address %q is not a valid URL.", d.Cfg.EngineURL), "Fix --engine-url."}}, false
	}
	if d.Cfg.EngineToken != "" {
		req.Header.Set("Authorization", "Bearer "+d.Cfg.EngineToken)
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return nil, []Finding{{"Engine", LevelProblem,
			fmt.Sprintf("The inference engine does not answer at %s.", d.Cfg.EngineURL),
			"Start it, or point --engine-url at it. With install-node.sh: macOS `launchctl print gui/$(id -u)/com.ktms.ollama`, Linux `systemctl --user status ktms-ollama`; its Ollama listens on 127.0.0.1:11435, a stock Ollama on 11434. (" + shortErr(err) + ")"}}, false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, []Finding{{"Engine", LevelProblem, fmt.Sprintf("The engine at %s refuses this agent (HTTP %d).", d.Cfg.EngineURL, resp.StatusCode), "Set NODE_AUTH_TOKEN to the token the engine expects."}}, false
	case resp.StatusCode >= 500:
		return nil, []Finding{{"Engine", LevelProblem, fmt.Sprintf("The engine at %s is failing (HTTP %d).", d.Cfg.EngineURL, resp.StatusCode), "Check the engine's own log."}}, false
	}

	models, err = GetInspector(d.Cfg.Engine).Inspect(ctx, d.Cfg.EngineURL, d.Cfg.EngineToken)
	models = FilterModels(models, d.Cfg.OnlyModels)
	if err != nil {
		return nil, []Finding{{"Engine", LevelProblem, "The engine answers but its model list cannot be read: " + shortErr(err), "Check that --engine matches what is running at " + d.Cfg.EngineURL + "."}}, false
	}
	return models, []Finding{{"Engine", LevelOK, fmt.Sprintf("The engine answers at %s.", d.Cfg.EngineURL), ""}}, true
}

func (d *Doctor) checkModel(models []*pb.NodeModelSpec) []Finding {
	if len(models) == 0 {
		fix := "Download one, for example: ollama pull <model>."
		if d.Model != "" {
			fix = "Download it: ollama pull " + d.Model + "."
		}
		return []Finding{{"Model", LevelProblem, "The engine is running but has no models, so the proxy has nothing to route here.", fix}}
	}
	if d.Model == "" {
		names := make([]string, 0, len(models))
		for _, m := range models {
			names = append(names, m.GetModelId())
		}
		return []Finding{{"Model", LevelOK, fmt.Sprintf("%d model(s) available: %s.", len(models), strings.Join(names, ", ")), ""}}
	}
	for _, m := range models {
		if sameModel(m.GetModelId(), d.Model) || sameModel(m.GetRemoteModelName(), d.Model) {
			if !m.GetIsWarm() {
				return []Finding{{"Model", LevelWarn, fmt.Sprintf("%s is installed but not loaded in memory. The first request will be slow and the proxy may prefer other nodes.", d.Model),
					`Load it and keep it loaded: curl ` + d.Cfg.EngineURL + `/api/generate -d '{"model":"` + d.Model + `","keep_alive":-1}'`}}
			}
			return []Finding{{"Model", LevelOK, fmt.Sprintf("%s is installed and loaded.", d.Model), ""}}
		}
	}
	return []Finding{{"Model", LevelProblem, fmt.Sprintf("The model %q is not installed on this engine.", d.Model), "Download it: ollama pull " + d.Model + "."}}
}

func sameModel(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	norm := func(s string) string { return strings.TrimSuffix(s, ":latest") }
	return norm(a) == norm(b)
}

// ---- acceleration ---------------------------------------------------------

type ollamaPS struct {
	Models []struct {
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		SizeVRAM int64  `json:"size_vram"`
	} `json:"models"`
}

func (d *Doctor) intelOllamaOnPath() (string, bool) {
	if d.GOOS != "darwin" || d.GOARCH != "arm64" {
		return "", false
	}
	p, err := d.LookPath("ollama")
	if err != nil {
		return "", false
	}
	archs, err := d.MachOArchs(p)
	if err != nil || len(archs) == 0 {
		return "", false
	}
	for _, a := range archs {
		if a == "arm64" {
			return "", false
		}
	}
	return p, true
}

func (d *Doctor) checkAcceleration(ctx context.Context) []Finding {
	if !strings.EqualFold(d.Cfg.Engine, "ollama") {
		return nil
	}
	intel, isIntel := d.intelOllamaOnPath()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, engineBaseURL(d.Cfg.EngineURL)+"/api/ps", nil)
	if err != nil {
		return nil
	}
	if d.Cfg.EngineToken != "" {
		req.Header.Set("Authorization", "Bearer "+d.Cfg.EngineToken)
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return nil // reported by the engine check
	}
	defer resp.Body.Close()
	var ps ollamaPS
	if json.NewDecoder(resp.Body).Decode(&ps) != nil || len(ps.Models) == 0 {
		if isIntel {
			return []Finding{{"GPU", LevelWarn, "An Intel-only Ollama is on PATH at " + intel + ". On Apple Silicon it runs through Rosetta, without the GPU.", "Use the native build: re-run install-node.sh, or install Ollama from https://ollama.com/download."}}
		}
		return nil
	}
	var out []Finding
	for _, m := range ps.Models {
		switch {
		case m.Size > 0 && m.SizeVRAM == 0:
			msg := fmt.Sprintf("%s is running on the CPU only, so generation will be very slow.", m.Name)
			fix := "Check that the engine can see the GPU (NVIDIA drivers on Linux)."
			if d.GOOS == "darwin" && d.GOARCH == "arm64" {
				msg = fmt.Sprintf("%s is running on the CPU only. On Apple Silicon this usually means an Intel build of Ollama running through Rosetta.", m.Name)
				fix = "Use the native Ollama: re-run install-node.sh, or install it from https://ollama.com/download, then restart the engine."
				if isIntel {
					msg += " The Ollama on PATH (" + intel + ") is such a build."
				}
			}
			out = append(out, Finding{"GPU", LevelProblem, msg, fix})
		case m.Size > 0 && m.SizeVRAM < m.Size:
			out = append(out, Finding{"GPU", LevelWarn, fmt.Sprintf("%s does not fit in GPU memory: %d%% is on the GPU, the rest runs on the CPU and slows it down.", m.Name, m.SizeVRAM*100/m.Size), "Use a smaller or more compressed model, or lower the context size."})
		default:
			out = append(out, Finding{"GPU", LevelOK, fmt.Sprintf("%s is fully loaded on the GPU.", m.Name), ""})
		}
	}
	return out
}

// machoArchs lists the CPU types in a Mach-O file, thin or universal.
func machoArchs(path string) ([]string, error) {
	name := func(c macho.Cpu) string {
		switch c {
		case macho.CpuAmd64:
			return "x86_64"
		case macho.CpuArm64:
			return "arm64"
		}
		return c.String()
	}
	if fat, err := macho.OpenFat(path); err == nil {
		defer fat.Close()
		var out []string
		for _, a := range fat.Arches {
			out = append(out, name(a.Cpu))
		}
		return out, nil
	}
	f, err := macho.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return []string{name(f.Cpu)}, nil
}

// ---- proxy and key --------------------------------------------------------

var boundToRE = regexp.MustCompile(`bound to node_id "([^"]*)"`)

func (d *Doctor) checkProxy(ctx context.Context) []Finding {
	host, _, err := net.SplitHostPort(d.Cfg.ProxyAddr)
	if err != nil {
		return []Finding{{"Proxy", LevelProblem, fmt.Sprintf("The proxy address %q should look like host:port.", d.Cfg.ProxyAddr), "Fix --proxy-addr."}}
	}
	if net.ParseIP(host) == nil {
		if _, err := net.DefaultResolver.LookupHost(ctx, host); err != nil {
			return []Finding{{"Proxy", LevelProblem, fmt.Sprintf("The name %s cannot be resolved.", host), "Check --proxy-addr for typos, and this machine's DNS and network. (" + shortErr(err) + ")"}}
		}
	}
	// Behind a CONNECT proxy a direct dial fails though the agent works; the gRPC call below tests the real path.
	if !d.UsesProxy(d.Cfg.ProxyAddr) {
		if err := d.DialTCP(ctx, d.Cfg.ProxyAddr); err != nil {
			return []Finding{{"Proxy", LevelProblem, fmt.Sprintf("Cannot connect to the proxy at %s.", d.Cfg.ProxyAddr), "Check the address and port, a VPN or firewall, and that the proxy is up. (" + shortErr(err) + ")"}}
		}
	}

	conn, err := grpclib.DialContext(ctx, d.Cfg.ProxyAddr, grpclib.WithTransportCredentials(NewAgent(d.Cfg).dialCredentials()))
	if err != nil {
		return []Finding{{"Proxy", LevelProblem, "Cannot set up a connection to the proxy: " + shortErr(err), ""}}
	}
	defer conn.Close()

	callCtx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+d.Cfg.NodeKey), 10*time.Second)
	defer cancel()
	_, err = pb.NewNodeRegistryServiceClient(conn).Heartbeat(callCtx, &pb.HeartbeatRequest{NodeId: probeNodeID})
	if err == nil {
		return []Finding{
			{"Proxy", LevelOK, fmt.Sprintf("The proxy at %s is reachable.", d.Cfg.ProxyAddr), ""},
			{"Node key", LevelOK, "The proxy accepts the node key.", ""},
		}
	}

	st, _ := status.FromError(err)
	reach := Finding{"Proxy", LevelOK, fmt.Sprintf("The proxy at %s is reachable.", d.Cfg.ProxyAddr), ""}
	switch st.Code() {
	case codes.Unauthenticated:
		return []Finding{reach, {"Node key", LevelProblem, "The proxy rejected the node key: it is wrong, revoked or expired.", "Ask for a new key and re-run install-node.sh with --new-key (or fix the file given to --node-key-file)."}}
	case codes.PermissionDenied:
		if m := boundToRE.FindStringSubmatch(st.Message()); m != nil {
			if m[1] == d.Cfg.NodeID {
				return []Finding{reach, {"Node key", LevelOK, fmt.Sprintf("The key is valid and bound to this node id (%s).", m[1]), ""}}
			}
			return []Finding{reach, {"Node key", LevelProblem, fmt.Sprintf("This key belongs to node %q but this agent runs as %q.", m[1], d.Cfg.NodeID), fmt.Sprintf("Run with --node-id %s, or ask for a key minted for %s.", m[1], d.Cfg.NodeID)}}
		}
		return []Finding{reach, {"Node key", LevelProblem, "The proxy refused the key for this operation: it is probably not a node key.", "Use a key minted with MintNodeKey (starts with ktms_node_)."}}
	case codes.Unavailable:
		return []Finding{{"Proxy", LevelProblem, "The proxy is not reachable over gRPC: " + transportHint(d.Cfg.TLS, st.Message()), ""}}
	case codes.DeadlineExceeded:
		return []Finding{{"Proxy", LevelProblem, "The proxy did not answer in time.", "Check the network path (a load balancer must pass /ktms.v1.NodeRegistryService/*)."}}
	}
	return []Finding{reach, {"Node key", LevelWarn, fmt.Sprintf("Unexpected answer from the proxy (%s): %s", st.Code(), st.Message()), ""}}
}

func hasProblem(fs []Finding) bool {
	for _, f := range fs {
		if f.Level == LevelProblem {
			return true
		}
	}
	return false
}

// checkRegistration asks the proxy to vet the exact models the agent would report, so a key
// whose allowed_models excludes one of them is caught here rather than at registration.
func (d *Doctor) checkRegistration(ctx context.Context, models []*pb.NodeModelSpec) []Finding {
	conn, err := grpclib.DialContext(ctx, d.Cfg.ProxyAddr, grpclib.WithTransportCredentials(NewAgent(d.Cfg).dialCredentials()))
	if err != nil {
		return nil // checkProxy already reported the connection
	}
	defer conn.Close()
	callCtx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+d.Cfg.NodeKey), 10*time.Second)
	defer cancel()
	resp, err := pb.NewNodeRegistryServiceClient(conn).ValidateNode(callCtx, &pb.ValidateNodeRequest{NodeId: d.Cfg.NodeID, Models: models})
	if err == nil {
		if dropped := resp.GetDroppedModels(); len(dropped) > 0 {
			return []Finding{{"Registration", LevelWarn,
				fmt.Sprintf("The node key does not allow %s, which this agent would report; the proxy ignores them.", strings.Join(dropped, ", ")),
				"Report only the allowed models: --only-model " + strings.Join(resp.GetAcceptedModels(), " --only-model ") + " (install-node.sh does this for its --model)."}}
		}
		return []Finding{{"Registration", LevelOK, fmt.Sprintf("The proxy would accept this node with %d model(s).", len(resp.GetAcceptedModels())), ""}}
	}
	st, _ := status.FromError(err)
	switch st.Code() {
	case codes.Unimplemented:
		return []Finding{{"Registration", LevelWarn, "This proxy is too old for a registration pre-check, so the models are not verified against the key's allowed models.", "Upgrade the proxy."}}
	case codes.PermissionDenied, codes.Unauthenticated, codes.InvalidArgument:
		return []Finding{{"Registration", LevelProblem, "The proxy would refuse to register this node: " + st.Message(),
			"Report only models the key allows with --only-model, use the node id the key is bound to, or ask for a key that fits."}}
	}
	return []Finding{{"Registration", LevelWarn, fmt.Sprintf("The registration pre-check failed (%s): %s", st.Code(), st.Message()), ""}}
}

// transportHint turns a gRPC transport failure into the likely cause.
func transportHint(usedTLS bool, msg string) string {
	switch {
	case strings.Contains(msg, "first record does not look like a TLS handshake"):
		return "this address speaks plain gRPC, not TLS. Drop --tls."
	case !usedTLS && (strings.Contains(msg, "error reading server preface") || strings.Contains(msg, "connection closed before server preface")):
		return "the address most likely requires TLS (a load balancer on port 443). Add --tls."
	case strings.Contains(msg, "x509:") || strings.Contains(msg, "certificate"):
		return "this machine does not trust the proxy's certificate (a corporate TLS-inspecting proxy, or a wrong clock?). " + msg
	}
	return msg
}

func shortErr(err error) string {
	var ce *x509.UnknownAuthorityError
	if errors.As(err, &ce) {
		return "certificate not trusted"
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// ---- how the proxy reaches the node ---------------------------------------

func (d *Doctor) checkTunnel(ctx context.Context) []Finding {
	bin := d.Cfg.CloudflaredBin
	if bin == "" {
		p, err := d.LookPath("cloudflared")
		if err != nil {
			return []Finding{{"Tunnel", LevelProblem, "cloudflared is not installed, so the node cannot be made reachable.", "macOS: brew install cloudflared. Linux: re-run install-node.sh. Or use --expose=none with your own --endpoint."}}
		}
		bin = p
	}
	if err := d.RunCmd(ctx, bin, "--version"); err != nil {
		return []Finding{{"Tunnel", LevelProblem, fmt.Sprintf("cloudflared (%s) is there but does not start (%s).", bin, shortErr(err)), "On macOS it may be blocked by Gatekeeper: re-download it with curl or brew instead of a browser."}}
	}
	if err := d.DialTCP(ctx, "region1.v2.argotunnel.com:7844"); err != nil {
		return []Finding{{"Tunnel", LevelProblem, "The tunnel cannot come up: this network blocks outbound port 7844 to Cloudflare.", "Try another network, or ask IT to allow outbound TCP/UDP 7844. (" + shortErr(err) + ")"}}
	}
	return []Finding{{"Tunnel", LevelOK, "cloudflared starts and Cloudflare is reachable.", ""}}
}

func (d *Doctor) checkEndpoint(ctx context.Context) []Finding {
	if d.Cfg.Endpoint == "" || d.Cfg.Endpoint == d.Cfg.EngineURL {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(d.Cfg.Endpoint, "/")+"/v1/models", nil)
	if err != nil {
		return []Finding{{"Endpoint", LevelProblem, fmt.Sprintf("The endpoint %q is not a valid URL.", d.Cfg.Endpoint), "Fix --endpoint."}}
	}
	if d.Cfg.NodeAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+d.Cfg.NodeAuthToken)
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return []Finding{{"Endpoint", LevelProblem, fmt.Sprintf("The address the proxy is told to use (%s) does not answer from here.", d.Cfg.Endpoint), "A tunnel that moved or stopped is the usual cause. (" + shortErr(err) + ")"}}
	}
	resp.Body.Close()
	return []Finding{{"Endpoint", LevelOK, fmt.Sprintf("%s answers (HTTP %d).", d.Cfg.Endpoint, resp.StatusCode), ""}}
}

// ---- report ---------------------------------------------------------------

// Render prints the findings and returns how many are problems.
func Render(w io.Writer, findings []Finding) (problems int) {
	tag := map[Level]string{LevelOK: "[ ok ]", LevelWarn: "[warn]", LevelProblem: "[FAIL]"}
	for _, f := range findings {
		fmt.Fprintf(w, "%s %s: %s\n", tag[f.Level], f.Check, f.Message)
		if f.Fix != "" && f.Level != LevelOK {
			fmt.Fprintf(w, "       What to do: %s\n", f.Fix)
		}
		if f.Level == LevelProblem {
			problems++
		}
	}
	switch problems {
	case 0:
		fmt.Fprintln(w, "\nNo problems found.")
	case 1:
		fmt.Fprintln(w, "\n1 problem found.")
	default:
		fmt.Fprintf(w, "\n%d problems found.\n", problems)
	}
	return problems
}
