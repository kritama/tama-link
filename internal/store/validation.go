package store

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kritama/tama-link/internal/limits"
)

func validateArguments(arguments json.RawMessage, lim limits.Limits) error {
	if !json.Valid(arguments) {
		return errors.New("arguments are not valid JSON")
	}
	if int64(len(arguments)) > int64(lim.ArgumentsBytes) {
		return fmt.Errorf("arguments exceed %d bytes", lim.ArgumentsBytes)
	}
	if depth := jsonDepth(arguments); depth > lim.ArgumentDepth {
		return fmt.Errorf("arguments depth %d exceeds %d", depth, lim.ArgumentDepth)
	}
	return nil
}

// jsonDepth counts object and array nesting. The input is known-valid JSON,
// so only string and escape tracking is required here.
func jsonDepth(data []byte) int {
	depth, maximum := 0, 0
	inString, escaped := false, false
	for _, char := range data {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch char {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maximum {
				maximum = depth
			}
		case '}', ']':
			depth--
		}
	}
	return maximum
}
