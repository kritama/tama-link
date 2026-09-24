package tama2026

import (
	"fmt"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/upstream"
)

// paramHeaders derives the reviewed argument-header mappings from the
// pinned input schema. The client receives only this mapping.
func paramHeaders(schema []byte) ([]upstream.ParamHeader, error) {
	extracted, err := catalog.ParamHeaders(schema)
	if err != nil {
		return nil, fmt.Errorf("parameter headers: %w", err)
	}
	out := make([]upstream.ParamHeader, len(extracted))
	for i, header := range extracted {
		out[i] = upstream.ParamHeader{
			Name: header.Name,
			Path: append([]string(nil), header.Path...),
			Type: header.Type,
		}
	}
	return out, nil
}
