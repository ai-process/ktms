package latent

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// DefaultHashDims is the width of the local fallback encoder.
const DefaultHashDims = 512

// HashEncoder is a deterministic, in-process encoder built on signed feature
// hashing of words, word bigrams and character trigrams. It is lexical, not
// semantic: it keeps caching and guardrails alive when no JEPA encoder is up.
type HashEncoder struct {
	dims  int
	model string
}

// NewHashEncoder returns a local encoder producing dims-wide vectors.
func NewHashEncoder(dims int) *HashEncoder {
	if dims <= 0 {
		dims = DefaultHashDims
	}
	return &HashEncoder{dims: dims, model: fmt.Sprintf("hash-v1-%d", dims)}
}

// Model names the vector space; it changes whenever dims or features change.
func (e *HashEncoder) Model() string { return e.model }

// Encode never fails on content; it only honours context cancellation.
func (e *HashEncoder) Encode(ctx context.Context, texts []string) ([]Embedding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]Embedding, len(texts))
	for i, t := range texts {
		out[i] = Embedding{Model: e.model, Vector: e.vector(t)}
	}
	return out, nil
}

const (
	weightWord     = 1.0
	weightStopword = 0.2
	weightBigram   = 0.7
	weightTrigram  = 0.4
)

func (e *HashEncoder) vector(text string) []float32 {
	v := make([]float32, e.dims)
	tokens := Tokenize(text)
	for i, tok := range tokens {
		w := float32(weightWord)
		if stopwords[tok] {
			w = weightStopword
		}
		e.add(v, fnvString(fnvOffset^'w', tok), w)
		if i > 0 {
			e.add(v, fnvString(fnvString(fnvOffset^'b', tokens[i-1])^' ', tok), weightBigram)
		}
		e.addTrigrams(v, tok)
	}
	if len(tokens) == 0 {
		// Punctuation-only or empty input still needs a comparable, non-zero vector.
		e.add(v, fnvString(fnvOffset^'r', text), weightWord)
	}
	if err := Normalize(v); err != nil {
		v[0] = 1
	}
	return v
}

func (e *HashEncoder) addTrigrams(v []float32, tok string) {
	runes := make([]rune, 0, len(tok)+2)
	runes = append(runes, ' ')
	runes = append(runes, []rune(tok)...)
	runes = append(runes, ' ')
	for i := 0; i+3 <= len(runes); i++ {
		h := uint64(fnvOffset ^ 'c')
		for _, r := range runes[i : i+3] {
			h = fnvRune(h, r)
		}
		e.add(v, h, weightTrigram)
	}
}

func (e *HashEncoder) add(v []float32, h uint64, w float32) {
	idx := (h & 0x7fffffffffffffff) % uint64(e.dims)
	if h>>63 == 1 {
		w = -w
	}
	v[idx] += w
}

const (
	fnvOffset = 14695981039346656037
	fnvPrime  = 1099511628211
)

func fnvString(h uint64, s string) uint64 {
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime
	}
	return h
}

func fnvRune(h uint64, r rune) uint64 {
	for r != 0 {
		h ^= uint64(r & 0xff)
		h *= fnvPrime
		r >>= 8
	}
	return h
}

// Tokenize folds text the way an attacker would try to evade a matcher:
// compatibility forms, diacritics, zero-width runes and common homoglyphs all
// collapse to plain lowercase Latin before splitting on non-alphanumerics.
func Tokenize(text string) []string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range norm.NFKD.String(text) {
		switch {
		case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Cf, r):
			continue
		}
		r = unicode.ToLower(r)
		if c, ok := confusables[r]; ok {
			r = c
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return joinSpacedLetters(strings.Fields(b.String()))
}

// joinSpacedLetters undoes "i g n o r e"-style spacing: a run of three or more
// single-rune tokens becomes one word.
func joinSpacedLetters(tokens []string) []string {
	out := tokens[:0]
	for i := 0; i < len(tokens); {
		j := i
		for j < len(tokens) && utf8.RuneCountInString(tokens[j]) == 1 {
			j++
		}
		if j-i >= 3 {
			out = append(out, strings.Join(tokens[i:j], ""))
			i = j
			continue
		}
		if j == i {
			j++
		}
		out = append(out, tokens[i:j]...)
		i = j
	}
	return out
}

// confusables maps Cyrillic and Greek lookalikes onto Latin letters.
var confusables = map[rune]rune{
	'а': 'a', 'в': 'b', 'е': 'e', 'ё': 'e', 'к': 'k', 'м': 'm', 'н': 'h', 'о': 'o',
	'р': 'p', 'с': 'c', 'т': 't', 'у': 'y', 'х': 'x', 'і': 'i', 'ј': 'j', 'ѕ': 's',
	'α': 'a', 'β': 'b', 'ε': 'e', 'η': 'n', 'ι': 'i', 'κ': 'k', 'ν': 'v', 'ο': 'o',
	'ρ': 'p', 'τ': 't', 'υ': 'u', 'χ': 'x',
}

var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true, "of": true,
	"to": true, "in": true, "on": true, "for": true, "with": true, "is": true,
	"are": true, "was": true, "be": true, "it": true, "this": true, "that": true,
	"i": true, "you": true, "me": true, "my": true, "do": true, "does": true,
	"how": true, "what": true, "can": true, "please": true, "at": true, "by": true,
}
