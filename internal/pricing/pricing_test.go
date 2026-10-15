package pricing

import (
	"math"
	"testing"
	"time"

	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
)

func TestCostStandard(t *testing.T) {
	m := &proxydb.Model{PriceInPerMtok: 2.50, PriceOutPerMtok: 10.00}
	// 1M in at $2.50 + 500k out at $10 = $2.50 + $5.00.
	got := Cost(m, llm.TokensUsage{Input: 1_000_000, Output: 500_000}, ModeStandard)
	if want := int64(7_500_000_000); got != want {
		t.Fatalf("cost = %d, want %d", got, want)
	}
}

func TestCostRoundsUpPerSide(t *testing.T) {
	// $0.0375/Mtok is 37.5 nano-USD per token.
	m := &proxydb.Model{PriceInPerMtok: 0.0375, PriceOutPerMtok: 0.0375}
	if got := Cost(m, llm.TokensUsage{Input: 1}, ModeStandard); got != 38 {
		t.Fatalf("1 token = %d nano, want 38 (rounded up)", got)
	}
	if got := Cost(m, llm.TokensUsage{Input: 2}, ModeStandard); got != 75 {
		t.Fatalf("2 tokens = %d nano, want 75", got)
	}
	// Each side rounds on its own: 37.5 + 37.5 is billed as 38 + 38.
	if got := Cost(m, llm.TokensUsage{Input: 1, Output: 1}, ModeStandard); got != 76 {
		t.Fatalf("1+1 tokens = %d nano, want 76", got)
	}
}

func TestCostIsExactWhereFloatsDrift(t *testing.T) {
	// 0.1 + 0.2 != 0.3 in float64; in nano-USD the sum is exact.
	m := &proxydb.Model{PriceInPerMtok: 0.1, PriceOutPerMtok: 0.2}
	got := Cost(m, llm.TokensUsage{Input: 1_000_000, Output: 1_000_000}, ModeStandard)
	if got != 300_000_000 {
		t.Fatalf("cost = %d, want 300000000", got)
	}
}

func TestCostModes(t *testing.T) {
	m := &proxydb.Model{
		PriceInPerMtok: 1, PriceOutPerMtok: 4,
		PriceInPeakPerMtok: 2, PriceOutPeakPerMtok: 0, // no peak output rate: standard applies
		PriceInBatchPerMtok: 0, PriceOutBatchPerMtok: 1,
	}
	u := llm.TokensUsage{Input: 1_000_000, Output: 1_000_000}
	cases := map[Mode]int64{
		ModeStandard: 5 * NanoPerUSD,
		ModePeak:     6 * NanoPerUSD,
		ModeBatch:    NanoPerUSD/2 + NanoPerUSD, // in defaults to half standard
	}
	for mode, want := range cases {
		if got := Cost(m, u, mode); got != want {
			t.Errorf("mode %d: cost = %d, want %d", mode, got, want)
		}
	}
}

func TestCostZeroAndNil(t *testing.T) {
	if Cost(nil, llm.TokensUsage{Input: 10}, ModeStandard) != 0 {
		t.Fatal("nil model must cost 0")
	}
	free := &proxydb.Model{}
	if Cost(free, llm.TokensUsage{Input: 10, Output: 10}, ModeStandard) != 0 {
		t.Fatal("unpriced model must cost 0")
	}
	if Cost(&proxydb.Model{PriceInPerMtok: 1}, llm.TokensUsage{Input: -5}, ModeStandard) != 0 {
		t.Fatal("negative tokens must cost 0")
	}
}

func TestCostSaturatesInsteadOfOverflowing(t *testing.T) {
	m := &proxydb.Model{PriceInPerMtok: 1e9, PriceOutPerMtok: 1e9}
	got := Cost(m, llm.TokensUsage{Input: math.MaxInt32, Output: math.MaxInt32}, ModeStandard)
	if got != math.MaxInt64 {
		t.Fatalf("cost = %d, want saturation at MaxInt64", got)
	}
}

func TestModeAt(t *testing.T) {
	peaky := &proxydb.Model{PriceInPerMtok: 1, PriceInPeakPerMtok: 2}
	flat := &proxydb.Model{PriceInPerMtok: 1}
	at := func(h, min int) time.Time { return time.Date(2026, 10, 15, h, min, 0, 0, time.UTC) }

	cases := []struct {
		t    time.Time
		want Mode
	}{
		{at(0, 59), ModeStandard},
		{at(1, 0), ModePeak},
		{at(3, 59), ModePeak},
		{at(4, 0), ModeStandard},
		{at(6, 0), ModePeak},
		{at(9, 59), ModePeak},
		{at(10, 0), ModeStandard},
		{at(23, 0), ModeStandard},
	}
	for _, c := range cases {
		if got := ModeAt(peaky, c.t); got != c.want {
			t.Errorf("ModeAt(%s) = %d, want %d", c.t.Format("15:04"), got, c.want)
		}
		if got := ModeAt(flat, c.t); got != ModeStandard {
			t.Errorf("model without peak rates must stay standard at %s", c.t.Format("15:04"))
		}
	}
	// Peak windows are UTC regardless of the caller's zone.
	berlin := time.FixedZone("CEST", 2*3600)
	if got := ModeAt(peaky, time.Date(2026, 10, 15, 3, 30, 0, 0, berlin)); got != ModePeak {
		t.Fatalf("03:30 CEST is 01:30 UTC, want peak, got %d", got)
	}
}
