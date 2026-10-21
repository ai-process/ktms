package nodeagent

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// EngineLoad is the engine's own view of its work: requests it is generating
// and requests it has accepted but not started. The proxy adds Waiting to its
// queue estimate, so a node busy with other clients is not overbooked.
type EngineLoad struct {
	Running uint32
	Waiting uint32
}

// LoadProber is implemented by inspectors whose engine publishes its queue.
// Ollama has no such API.
type LoadProber interface {
	Load(ctx context.Context, endpoint, authToken string) (EngineLoad, error)
}

// Prometheus gauge names per engine: running first, waiting second.
var (
	vllmLoadMetrics     = [2]string{"vllm:num_requests_running", "vllm:num_requests_waiting"}
	tgiLoadMetrics      = [2]string{"tgi_batch_current_size", "tgi_queue_size"}
	llamaCppLoadMetrics = [2]string{"llamacpp:requests_processing", "llamacpp:requests_deferred"} // needs --metrics
)

func (vi *VLLMInspector) Load(ctx context.Context, endpoint, authToken string) (EngineLoad, error) {
	return scrapeLoad(ctx, endpoint, authToken, vllmLoadMetrics)
}

func (ti *TGIInspector) Load(ctx context.Context, endpoint, authToken string) (EngineLoad, error) {
	return scrapeLoad(ctx, endpoint, authToken, tgiLoadMetrics)
}

func (li *LlamaCppInspector) Load(ctx context.Context, endpoint, authToken string) (EngineLoad, error) {
	return scrapeLoad(ctx, endpoint, authToken, llamaCppLoadMetrics)
}

// scrapeLoad reads the engine's /metrics (served at the root, not under /v1)
// and sums the two gauges across label sets, e.g. one series per model.
func scrapeLoad(ctx context.Context, endpoint, authToken string, names [2]string) (EngineLoad, error) {
	base := engineBaseURL(endpoint)
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/metrics", nil)
	if err != nil {
		return EngineLoad{}, err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return EngineLoad{}, fmt.Errorf("engine /metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return EngineLoad{}, fmt.Errorf("engine /metrics: %s", resp.Status)
	}

	var sums [2]float64
	var seen [2]bool
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		name, value, ok := parseSample(sc.Text())
		if !ok {
			continue
		}
		for i, want := range names {
			if name == want {
				sums[i] += value
				seen[i] = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		return EngineLoad{}, fmt.Errorf("read engine /metrics: %w", err)
	}
	if !seen[0] && !seen[1] {
		return EngineLoad{}, fmt.Errorf("engine /metrics has neither %s nor %s", names[0], names[1])
	}
	return EngineLoad{Running: gauge(sums[0]), Waiting: gauge(sums[1])}, nil
}

// parseSample reads one Prometheus text sample: name{labels} value [timestamp].
func parseSample(line string) (string, float64, bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return "", 0, false
	}
	name, rest := line, ""
	if i := strings.IndexAny(line, "{ \t"); i >= 0 {
		name, rest = line[:i], line[i:]
	}
	if strings.HasPrefix(rest, "{") {
		end := strings.LastIndex(rest, "}")
		if end < 0 {
			return "", 0, false
		}
		rest = rest[end+1:]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return "", 0, false
	}
	return name, v, true
}

// gauge caps at MaxInt32: the proxy reads the field as int32, and a larger
// value would wrap negative and make a loaded node look idle.
func gauge(v float64) uint32 {
	if v <= 0 || v != v {
		return 0
	}
	if v >= math.MaxInt32 {
		return math.MaxInt32
	}
	return uint32(v + 0.5)
}

// engineBaseURL is the engine's root, where /metrics and native APIs live.
func engineBaseURL(endpoint string) string {
	return strings.TrimSuffix(strings.TrimRight(endpoint, "/"), "/v1")
}
