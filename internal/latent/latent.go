// Package latent projects text into a representation space (JEPA or any
// embedding encoder) for semantic caching and latent guardrails.
package latent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// ErrInvalidVector reports an encoder output that cannot be compared.
var ErrInvalidVector = errors.New("latent: invalid vector")

// Embedding is one unit-length vector and the encoder that produced it.
// Vectors from different models live in different spaces and are never compared.
type Embedding struct {
	Model  string
	Vector []float32
}

// Encoder maps texts to embeddings, one per input, in input order.
type Encoder interface {
	Encode(ctx context.Context, texts []string) ([]Embedding, error)
}

// Normalize scales v to unit length in place so cosine similarity is a dot product.
func Normalize(v []float32) error {
	var sum float64
	for _, x := range v {
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("%w: non-finite value", ErrInvalidVector)
		}
		sum += f * f
	}
	if sum == 0 {
		return fmt.Errorf("%w: zero norm", ErrInvalidVector)
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
	return nil
}

// Dot is the inner product of equal-length vectors; for unit vectors it is the
// cosine similarity. Mismatched lengths score 0 rather than panic.
func Dot(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= len(a); i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < len(a); i++ {
		s0 += a[i] * b[i]
	}
	return s0 + s1 + s2 + s3
}

// Cosine is the cosine similarity of arbitrary (not necessarily unit) vectors.
func Cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

// MarshalVector encodes v as little-endian float32s.
func MarshalVector(v []float32) []byte {
	out := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(x))
	}
	return out
}

// UnmarshalVector decodes the output of MarshalVector.
func UnmarshalVector(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("%w: %d bytes is not a float32 multiple", ErrInvalidVector, len(b))
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out, nil
}
