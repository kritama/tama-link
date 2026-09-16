package catalog

import (
	"encoding/json"
	"fmt"

	"github.com/cockroachdb/apd/v3"
)

// maxAcceptedExponent is the reviewed limit on the effective exponent of an
// accepted JSON number. It is the exponent range of the arbitrary-precision
// decimal representation (apd/v3) used for all exact numeric work: exponents
// outside [-maxAcceptedExponent, +maxAcceptedExponent] are rejected through
// the normal validation error path at profile load (numeric bounds) or
// instance validation (whenever a numeric assertion must evaluate the
// value). No path ever allocates in proportion to the exponent magnitude.
const maxAcceptedExponent = apd.MaxExponent

// parseJSONNumber parses one JSON number literal into exact
// arbitrary-precision decimal form. Callers must have already passed the
// literal through isJSONNumber, which enforces the complete JSON number
// grammar; this function then performs the exact decimal conversion and
// reports exponents outside the reviewed range as errors instead of
// panicking or requesting exponent-sized allocations.
func parseJSONNumber(text json.RawMessage) (*apd.Decimal, error) {
	dec, _, err := apd.NewFromString(string(trimJSON(text)))
	if err != nil {
		return nil, fmt.Errorf("number is outside the reviewed decimal exponent range of ±%d", maxAcceptedExponent)
	}
	return dec, nil
}

// jsonInstanceEqual reports whether two JSON literals are the same instance
// value under JSON Schema instance equality: numbers compare by exact
// mathematical value (1, 1.0, and 1e0 are equal), strings by decoded code
// points, arrays positionally, and objects independently of key order. The
// comparison recurses through arrays and objects; numeric leaves anywhere in
// the tree use the same exact decimal model as numeric bounds. A non-nil
// error means a numeric leaf could not be evaluated (an out-of-range
// exponent); the caller translates it into a validation result.
func jsonInstanceEqual(a, b json.RawMessage) (bool, error) {
	ta, tb := trimJSON(a), trimJSON(b)
	ka, kb := valueKind(ta), valueKind(tb)
	if ka != kb {
		return false, nil
	}
	switch ka {
	case jsonKindNull, jsonKindBoolean:
		return string(ta) == string(tb), nil
	case jsonKindString:
		var sa, sb string
		if err := json.Unmarshal(ta, &sa); err != nil {
			return false, err
		}
		if err := json.Unmarshal(tb, &sb); err != nil {
			return false, err
		}
		return sa == sb, nil
	case jsonKindNumber:
		da, err := parseJSONNumber(ta)
		if err != nil {
			return false, err
		}
		db, err := parseJSONNumber(tb)
		if err != nil {
			return false, err
		}
		return da.Cmp(db) == 0, nil
	case jsonKindArray:
		var la, lb []json.RawMessage
		if err := json.Unmarshal(ta, &la); err != nil {
			return false, err
		}
		if err := json.Unmarshal(tb, &lb); err != nil {
			return false, err
		}
		if len(la) != len(lb) {
			return false, nil
		}
		for i := range la {
			eq, err := jsonInstanceEqual(la[i], lb[i])
			if err != nil {
				return false, err
			}
			if !eq {
				return false, nil
			}
		}
		return true, nil
	case jsonKindObject:
		var ma, mb map[string]json.RawMessage
		if err := json.Unmarshal(ta, &ma); err != nil {
			return false, err
		}
		if err := json.Unmarshal(tb, &mb); err != nil {
			return false, err
		}
		if len(ma) != len(mb) {
			return false, nil
		}
		for key, va := range ma {
			vb, ok := mb[key]
			if !ok {
				return false, nil
			}
			eq, err := jsonInstanceEqual(va, vb)
			if err != nil {
				return false, err
			}
			if !eq {
				return false, nil
			}
		}
		return true, nil
	}
	return false, nil
}
