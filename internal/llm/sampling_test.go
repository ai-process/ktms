package llm

import (
	"slices"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func TestSamplingSupportApply(t *testing.T) {
	all := Sampling{Temperature: ptr(1.4), TopP: ptr(0.9), Stop: []string{"a", "b", "c"}, Seed: ptr(int64(7))}
	cases := []struct {
		name    string
		support SamplingSupport
		in      Sampling
		want    Sampling
		ignored []string
	}{
		{"takes all", AllSampling, all, all, nil},
		{"takes nothing", SamplingSupport{}, all, Sampling{}, []string{ParamTemperature, ParamTopP, ParamStop, ParamSeed}},
		{"unset stays unset", SamplingSupport{}, Sampling{}, Sampling{}, nil},
		{"temperature capped and reported", SamplingSupport{Temperature: true, MaxTemperature: 1},
			Sampling{Temperature: ptr(1.4)}, Sampling{Temperature: ptr(1.0)}, []string{ParamTemperature}},
		{"temperature within the cap", SamplingSupport{Temperature: true, MaxTemperature: 1},
			Sampling{Temperature: ptr(0.4)}, Sampling{Temperature: ptr(0.4)}, nil},
		{"stop over the limit keeps the first and is reported", SamplingSupport{Stop: true, MaxStop: 2},
			Sampling{Stop: []string{"a", "b", "c"}}, Sampling{Stop: []string{"a", "b"}}, []string{ParamStop}},
		{"one of temperature and top_p keeps temperature", SamplingSupport{Temperature: true, TopP: true, OneOfTemperatureTopP: true},
			Sampling{Temperature: ptr(0.2), TopP: ptr(0.9)}, Sampling{Temperature: ptr(0.2)}, []string{ParamTopP}},
		{"blank stop sequences dropped and reported", SamplingSupport{Stop: true, NoBlankStop: true},
			Sampling{Stop: []string{"\n", "END"}}, Sampling{Stop: []string{"END"}}, []string{ParamStop}},
		{"only blank stop sequences", SamplingSupport{Stop: true, NoBlankStop: true},
			Sampling{Stop: []string{" "}}, Sampling{}, []string{ParamStop}},
		{"one of, top_p alone passes", SamplingSupport{Temperature: true, TopP: true, OneOfTemperatureTopP: true},
			Sampling{TopP: ptr(0.9)}, Sampling{TopP: ptr(0.9)}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ignored := c.support.Apply(c.in)
			if !sameSampling(got, c.want) || !slices.Equal(ignored, c.ignored) {
				t.Fatalf("got %+v %v, want %+v %v", got, ignored, c.want, c.ignored)
			}
		})
	}
}

func sameSampling(a, b Sampling) bool {
	eq := func(x, y *float64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	seed := (a.Seed == nil) == (b.Seed == nil) && (a.Seed == nil || *a.Seed == *b.Seed)
	return eq(a.Temperature, b.Temperature) && eq(a.TopP, b.TopP) && slices.Equal(a.Stop, b.Stop) && seed
}
