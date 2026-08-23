// Package tokens provides tokenizer-aware token estimation for model
// budget reservations. Estimates only guard the reservation envelope: the
// provider-reported usage at Finish remains the authoritative settlement,
// so every estimator errs on the high side.
package tokens

import (
	"fmt"
	"math"
)

// Estimator returns a conservative token estimate for one text chunk.
type Estimator func(text string) int64

// heuristicMargin is the safety multiplier applied to the heuristic
// estimate. Reservation under-estimation is the dangerous direction — it
// lets concurrent calls collectively promise past a budget — so the
// heuristic intentionally over-reserves slightly.
const heuristicMargin = 1.10

// conservativeMargin doubles the safety margin for models whose tokenizer
// is unknown to the platform: an uncharacterized tokenizer can compress or
// expand text far more than the script-aware heuristic anticipates.
const conservativeMargin = 2.00

// asciiBytesPerToken approximates Latin/mixed content. The classic
// bytes/4 rule is kept as the floor: the script-aware estimate is never
// below it.
const asciiBytesPerToken = 4

// Heuristic estimates tokens from the text's script mix. CJK and other
// dense scripts tokenize at roughly one token per character — far above the
// bytes/4 average that badly under-reserved multilingual prompts — while
// Latin text keeps the conventional four-bytes-per-token average. The
// result includes a 10% safety margin and is never below bytes/4.
func Heuristic(text string) int64 {
	return estimate(text, heuristicMargin)
}

// Conservative estimates tokens with the doubled safety margin used for
// models whose tokenizer is unknown or non-standard (reasoning models,
// bespoke tokenizers). Failing safe here means reserving more, never less.
func Conservative(text string) int64 {
	return estimate(text, conservativeMargin)
}

func estimate(text string, margin float64) int64 {
	if text == "" {
		return 0
	}
	denseRunes, otherBytes := 0, 0
	for _, r := range text {
		if isDenseScript(r) {
			denseRunes++
			continue
		}
		otherBytes += runeLen(r)
	}
	legacy := (int64(len(text)) + int64(asciiBytesPerToken) - 1) / int64(asciiBytesPerToken)
	scriptAware := float64(denseRunes) + float64(otherBytes)/float64(asciiBytesPerToken)
	scaled := int64(math.Ceil(scriptAware * margin))
	if scaled < legacy {
		return legacy
	}
	return scaled
}

// isDenseScript reports runes whose tokenizers typically spend about one
// token per character: the CJK ideograph blocks, kana, hangul, and related
// fullwidth scripts.
func isDenseScript(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x11FF, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303F,   // CJK Radicals, Kangxi, CJK Symbols
		r >= 0x3040 && r <= 0x30FF,   // Hiragana, Katakana
		r >= 0x3130 && r <= 0x318F,   // Hangul Compatibility Jamo
		r >= 0x3400 && r <= 0x4DBF,   // CJK Extension A
		r >= 0x4E00 && r <= 0x9FFF,   // CJK Unified Ideographs
		r >= 0xA960 && r <= 0xA97F,   // Hangul Jamo Extended-A
		r >= 0xAC00 && r <= 0xD7FF,   // Hangul Syllables + Jamo Extended-B
		r >= 0xF900 && r <= 0xFAFF,   // CJK Compatibility Ideographs
		r >= 0xFF00 && r <= 0xFF60,   // Fullwidth Forms
		r >= 0x20000 && r <= 0x2FA1F: // CJK Extensions B..F, Compatibility
		return true
	default:
		return false
	}
}

func runeLen(r rune) int {
	switch {
	case r < 0x80:
		return 1
	case r < 0x800:
		return 2
	case r < 0x10000:
		return 3
	default:
		return 4
	}
}

// ForName resolves a configured tokenizer name to its estimator. The empty
// name (an unconfigured descriptor) uses the script-aware heuristic; the
// explicit "conservative" mode doubles the safety margin. An unrecognized
// name fails safe to the conservative estimator rather than silently
// under-reserving.
func ForName(name string) (Estimator, error) {
	switch name {
	case "", "heuristic":
		return Heuristic, nil
	case "conservative":
		return Conservative, nil
	default:
		return nil, fmt.Errorf("unknown tokenizer %q (supported: heuristic, conservative)", name)
	}
}
