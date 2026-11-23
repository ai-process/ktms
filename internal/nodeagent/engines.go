package nodeagent

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/rs/zerolog/log"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// EngineSpec is an inference engine a node serves next to its primary one,
// such as ktms-mt beside Ollama. The gateway routes each request by model.
type EngineSpec struct {
	Name string
	URL  string
}

// ParseEngineSpec reads NAME=URL, e.g. mt=http://127.0.0.1:11436.
func ParseEngineSpec(v string) (EngineSpec, error) {
	name, raw, ok := strings.Cut(strings.TrimSpace(v), "=")
	name = strings.ToLower(strings.TrimSpace(name))
	if !ok || name == "" || raw == "" {
		return EngineSpec{}, fmt.Errorf("%q: want ENGINE=URL, e.g. mt=http://127.0.0.1:11436", v)
	}
	if u, err := url.Parse(raw); err != nil || u.Scheme == "" || u.Host == "" {
		return EngineSpec{}, fmt.Errorf("%q: %q is not an http(s) URL", v, raw)
	}
	return EngineSpec{Name: name, URL: raw}, nil
}

// nodeEngine is one engine the agent inspects; the primary comes first.
type nodeEngine struct {
	name, url string
	inspector EngineInspector
}

func (a *Agent) engineList() []nodeEngine {
	out := []nodeEngine{{name: strings.ToLower(a.cfg.Engine), url: a.cfg.EngineURL, inspector: a.inspector}}
	for _, e := range a.cfg.ExtraEngines {
		out = append(out, nodeEngine{name: e.Name, url: e.URL, inspector: GetInspector(e.Name)})
	}
	return out
}

// engineView is what the last inspection found, for the status and the gateway.
type engineView struct {
	states  []StatusEngine
	byModel map[string]string // engine model id or remote name -> engine name
}

// inspectEngines lists the models of every engine. A paused engine's models
// are left out of the report, so the proxy stops routing to them, but stay in
// byModel: the gateway then refuses them instead of sending them elsewhere.
func (a *Agent) inspectEngines(ctx context.Context) []*pb.NodeModelSpec {
	engines := a.engineList()
	paused := a.pausedEngines()
	view := engineView{byModel: map[string]string{}}
	var out []*pb.NodeModelSpec
	seen := map[string]bool{}
	for i, e := range engines {
		models, err := e.inspector.Inspect(ctx, e.url, a.cfg.EngineToken)
		st := StatusEngine{Name: e.name, Primary: i == 0, Up: err == nil, Paused: slices.Contains(paused, e.name)}
		if err != nil {
			st.Error = shortErr(err)
			msg := "engine inspection failed, reporting no models"
			if len(engines) > 1 {
				msg = "engine inspection failed, reporting none of its models"
			}
			log.Warn().Err(err).Str("engine", e.name).Msg(msg)
		}
		for _, m := range models {
			id := m.GetModelId()
			if seen[id] {
				log.Warn().Str("model", id).Str("engine", e.name).Msg("another engine already serves this model id; ignored here")
				continue
			}
			seen[id] = true
			view.byModel[id] = e.name
			if r := m.GetRemoteModelName(); r != "" {
				view.byModel[r] = e.name
			}
			st.Models = append(st.Models, id)
			if !st.Paused {
				out = append(out, m)
			}
		}
		view.states = append(view.states, st)
	}
	a.mu.Lock()
	a.engines = view
	gw := a.gateway
	a.mu.Unlock()
	if gw != nil {
		gw.SetRoutes(view.byModel, paused)
	}
	return out
}

// engineOf names the engine that serves model; "" when unknown.
func (a *Agent) engineOf(model string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.engines.byModel[model]
}

func (a *Agent) engineStates() []StatusEngine {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.engines.states)
}

func (a *Agent) pausedEngines() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.enginesPaused)
}

type enginePauseFile struct {
	Paused []string `json:"paused"`
}

// loadEnginePauses restores the engines the owner paused before a restart.
func (a *Agent) loadEnginePauses() {
	var f enginePauseFile
	if ok, err := readJSON(a.stateFile("engines.json"), &f); err != nil {
		log.Warn().Err(err).Msg("cannot read the paused engines; all engines serve")
		return
	} else if !ok {
		return
	}
	var keep []string
	for _, e := range a.engineList() {
		if slices.Contains(f.Paused, e.name) {
			keep = append(keep, e.name)
		}
	}
	a.mu.Lock()
	a.enginesPaused = keep
	a.mu.Unlock()
	if len(keep) > 0 {
		log.Info().Strs("engines", keep).Msg("engines paused by the owner stay paused")
	}
}

// setEnginePaused pauses or resumes one engine of a node with several; the
// node and its other engines keep serving.
func (a *Agent) setEnginePaused(name string, paused bool) error {
	name = strings.ToLower(name)
	engines := a.engineList()
	if len(engines) < 2 {
		return badRequest("this node runs one engine: pause the node instead")
	}
	if !slices.ContainsFunc(engines, func(e nodeEngine) bool { return e.name == name }) {
		return badRequest("no engine %q on this node", name)
	}
	a.mu.Lock()
	cur := slices.DeleteFunc(slices.Clone(a.enginesPaused), func(s string) bool { return s == name })
	if paused {
		cur = append(cur, name)
	}
	if len(cur) == len(engines) {
		a.mu.Unlock()
		return badRequest("that would pause every engine: pause the node instead")
	}
	a.enginesPaused = cur
	byModel, gw := a.engines.byModel, a.gateway
	a.mu.Unlock()
	if gw != nil {
		gw.SetRoutes(byModel, cur)
	}
	verb := "resumed"
	if paused {
		verb = "paused"
	}
	log.Info().Str("engine", name).Msg("engine " + verb + " by the owner")
	return writeJSON0600(a.stateFile("engines.json"), enginePauseFile{Paused: cur})
}

// primaryEngineModel tells models of the primary engine, the only one the
// schedule can unload and warm; an unknown model is taken as the primary's.
func (a *Agent) primaryEngineModel(model string) bool {
	e := a.engineOf(model)
	return e == "" || e == strings.ToLower(a.cfg.Engine)
}
