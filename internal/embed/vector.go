package embed

import (
	"math"
	"strconv"
)

// MaxTextRunes caps the text sent for one item or query. The sidecar truncates
// at 2048 tokens anyway; the cap bounds request size and time (one 6000-character
// text takes about 2.4 s on the 4-CPU sidecar).
const MaxTextRunes = 6000

// Clip cuts text to MaxTextRunes characters.
func Clip(text string) string {
	n := 0
	for i := range text {
		if n == MaxTextRunes {
			return text[:i]
		}
		n++
	}
	return text
}

// ItemText is the text embedded for a knowledge item (evals/retrieval: title + "\n" + body).
func ItemText(title, body string) string { return Clip(title + "\n" + body) }

// Literal renders a vector as a pgvector text literal, to be bound as
// $n::text::vector so that pgx needs no pgvector type support.
func Literal(v []float32) string {
	b := make([]byte, 0, len(v)*12+2)
	b = append(b, '[')
	for i, f := range v {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendFloat(b, float64(f), 'g', -1, 32)
	}
	return string(append(b, ']'))
}

// normalize scales v to unit length in place (the sidecar already does; this
// keeps the stored invariant). It reports false for a zero or non-finite vector.
func normalize(v []float32) bool {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	norm := math.Sqrt(sum)
	if !(norm > 0) || math.IsInf(norm, 0) {
		return false
	}
	if math.Abs(norm-1) > 1e-4 {
		for i := range v {
			v[i] = float32(float64(v[i]) / norm)
		}
	}
	return true
}
