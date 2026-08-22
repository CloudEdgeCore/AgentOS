// Package money provides the fixed-point representation used by every
// accounting ledger. One unit is one millionth of a US dollar (microUSD).
package money

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

const MicroPerUSD int64 = 1_000_000

// MicroUSD is an exact, non-floating-point monetary amount.
type MicroUSD int64

// FromUSD converts an API-boundary dollar value into fixed-point microUSD.
// Values are rounded to the public contract's six-decimal precision.
func FromUSD(value float64) (MicroUSD, error) {
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) || value > float64(math.MaxInt64)/float64(MicroPerUSD) {
		return 0, errors.New("USD amount is outside the supported range")
	}
	return MicroUSD(math.Round(value * float64(MicroPerUSD))), nil
}

// MustFromUSD is intended for trusted constants and tests.
func MustFromUSD(value float64) MicroUSD {
	amount, err := FromUSD(value)
	if err != nil {
		panic(err)
	}
	return amount
}

func (amount MicroUSD) USD() float64 {
	return float64(amount) / float64(MicroPerUSD)
}

// MarshalJSON preserves the existing public costUsd contract while keeping
// the in-memory representation fixed-point.
func (amount MicroUSD) MarshalJSON() ([]byte, error) {
	if amount < 0 {
		return nil, errors.New("microUSD amount must not be negative")
	}
	whole := int64(amount) / MicroPerUSD
	fraction := int64(amount) % MicroPerUSD
	if fraction == 0 {
		return []byte(strconv.FormatInt(whole, 10)), nil
	}
	return []byte(fmt.Sprintf("%d.%s", whole, strings.TrimRight(fmt.Sprintf("%06d", fraction), "0"))), nil
}

// UnmarshalJSON accepts a JSON dollar number with at most six decimal places.
func (amount *MicroUSD) UnmarshalJSON(encoded []byte) error {
	if amount == nil {
		return errors.New("microUSD destination is nil")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var number json.Number
	if err := decoder.Decode(&number); err != nil {
		return errors.New("USD amount must be a JSON number")
	}
	rational, ok := new(big.Rat).SetString(number.String())
	if !ok || rational.Sign() < 0 {
		return errors.New("USD amount must be non-negative")
	}
	rational.Mul(rational, big.NewRat(MicroPerUSD, 1))
	if !rational.IsInt() || !rational.Num().IsInt64() {
		return errors.New("USD amount must fit int64 microUSD with at most six decimal places")
	}
	*amount = MicroUSD(rational.Num().Int64())
	return nil
}

// TokenCost calculates tokens * price-per-million-tokens without a floating
// intermediate. Fractions of one microUSD are rounded up so metering never
// silently undercharges a provider call.
func TokenCost(tokens int64, pricePerMillion MicroUSD) (MicroUSD, error) {
	if tokens < 0 || pricePerMillion < 0 {
		return 0, errors.New("tokens and price must not be negative")
	}
	numerator := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(int64(pricePerMillion)))
	if numerator.Sign() == 0 {
		return 0, nil
	}
	numerator.Add(numerator, big.NewInt(999_999))
	numerator.Quo(numerator, big.NewInt(1_000_000))
	if !numerator.IsInt64() {
		return 0, errors.New("calculated model cost overflows microUSD")
	}
	return MicroUSD(numerator.Int64()), nil
}

func Add(left, right MicroUSD) (MicroUSD, error) {
	if left < 0 || right < 0 || int64(left) > math.MaxInt64-int64(right) {
		return 0, errors.New("microUSD addition overflows")
	}
	return left + right, nil
}
