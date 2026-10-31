package llm

import (
	"slices"
	"strings"
)

// Sampling parameter names, as callers send them and responses report them.
const (
	ParamTemperature = "temperature"
	ParamTopP        = "top_p"
	ParamStop        = "stop"
	ParamSeed        = "seed"
)

// Sampling is the caller's sampling settings; a nil field or an empty Stop
// leaves the model's default.
type Sampling struct {
	Temperature *float64
	TopP        *float64
	Stop        []string
	Seed        *int64
}

// SamplingSupport is what one model takes. The zero value takes nothing, so a
// model nobody described never gets a field its API may reject.
type SamplingSupport struct {
	Temperature, TopP, Stop, Seed bool
	MaxTemperature                float64 // 0 means no cap below the caller's range
	MaxStop                       int     // 0 means no limit
	// OneOfTemperatureTopP: the API rejects both together, so top_p goes.
	OneOfTemperatureTopP bool
	// NoBlankStop: the API rejects a stop sequence of whitespace alone.
	NoBlankStop bool
}

// AllSampling is an OpenAI-style API that takes every field.
var AllSampling = SamplingSupport{Temperature: true, TopP: true, Stop: true, Seed: true}

// Apply returns the settings the model gets and the names of those it does
// not get as sent: a temperature lowered to the cap, a stop list cut to the
// limit or rid of blank sequences the API rejects, and every field it lacks.
func (s SamplingSupport) Apply(in Sampling) (Sampling, []string) {
	var out Sampling
	var ignored []string
	if in.Temperature != nil {
		if s.Temperature {
			t := *in.Temperature
			if s.MaxTemperature > 0 && t > s.MaxTemperature {
				t = s.MaxTemperature
				ignored = append(ignored, ParamTemperature)
			}
			out.Temperature = &t
		} else {
			ignored = append(ignored, ParamTemperature)
		}
	}
	if in.TopP != nil {
		if s.TopP && !(s.OneOfTemperatureTopP && out.Temperature != nil) {
			out.TopP = in.TopP
		} else {
			ignored = append(ignored, ParamTopP)
		}
	}
	if len(in.Stop) > 0 {
		stop := in.Stop
		if s.NoBlankStop {
			stop = slices.DeleteFunc(slices.Clone(stop), func(v string) bool { return strings.TrimSpace(v) == "" })
		}
		if s.Stop && s.MaxStop > 0 && len(stop) > s.MaxStop {
			stop = stop[:s.MaxStop]
		}
		if s.Stop && len(stop) > 0 {
			out.Stop = stop
		}
		if !s.Stop || len(stop) < len(in.Stop) {
			ignored = append(ignored, ParamStop)
		}
	}
	if in.Seed != nil {
		if s.Seed {
			out.Seed = in.Seed
		} else {
			ignored = append(ignored, ParamSeed)
		}
	}
	return out, ignored
}
