// Package pricing turns token usage into money. Amounts are int64 nano-USD
// (1e-9 USD) so budgets and usage events never accumulate float drift.
package pricing

import (
	"math"
	"math/bits"
	"time"

	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
)

// Mode selects which of a model's rates apply.
type Mode int

const (
	ModeStandard Mode = iota // off-peak interactive rate
	ModePeak                 // peak rate where the model has one, else standard
	ModeBatch                // batch rate, defaulting to half the standard rate
)

// NanoPerUSD converts USD to the internal unit.
const NanoPerUSD = 1_000_000_000

const tokensPerMtok = 1_000_000

// ModeAt is the interactive mode for a call starting at t. Peak rates exist
// only for DeepSeek V4 today, which doubles for UTC 01:00-04:00 and 06:00-10:00.
func ModeAt(m *proxydb.Model, t time.Time) Mode {
	if m == nil || (m.PriceInPeakPerMtok <= 0 && m.PriceOutPeakPerMtok <= 0) {
		return ModeStandard
	}
	h := t.UTC().Hour()
	if (h >= 1 && h < 4) || (h >= 6 && h < 10) {
		return ModePeak
	}
	return ModeStandard
}

// Cost prices usage on m in the given mode, rounding each side up so a
// budget is never under-charged by a fraction of a nano-USD.
func Cost(m *proxydb.Model, usage llm.TokensUsage, mode Mode) int64 {
	if m == nil {
		return 0
	}
	in, out := rates(m, mode)
	return addSat(tokensCost(int64(usage.Input), in), tokensCost(int64(usage.Output), out))
}

func rates(m *proxydb.Model, mode Mode) (in, out float64) {
	switch mode {
	case ModeBatch:
		return m.BatchPriceInPerMtok(), m.BatchPriceOutPerMtok()
	case ModePeak:
		in, out = m.PriceInPerMtok, m.PriceOutPerMtok
		if m.PriceInPeakPerMtok > 0 {
			in = m.PriceInPeakPerMtok
		}
		if m.PriceOutPeakPerMtok > 0 {
			out = m.PriceOutPeakPerMtok
		}
		return in, out
	default:
		return m.PriceInPerMtok, m.PriceOutPerMtok
	}
}

// tokensCost is ceil(tokens * price_nano_per_mtok / 1e6) in 128-bit
// arithmetic; the per-Mtok price is rounded to whole nano-USD first.
func tokensCost(tokens int64, pricePerMtok float64) int64 {
	if tokens <= 0 || pricePerMtok <= 0 {
		return 0
	}
	nanoPerMtok := math.Round(pricePerMtok * NanoPerUSD)
	if nanoPerMtok >= math.MaxInt64 {
		return math.MaxInt64
	}
	hi, lo := bits.Mul64(uint64(tokens), uint64(nanoPerMtok))
	if hi >= tokensPerMtok {
		return math.MaxInt64
	}
	q, r := bits.Div64(hi, lo, tokensPerMtok)
	if r != 0 {
		q++
	}
	if q > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(q)
}

func addSat(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
