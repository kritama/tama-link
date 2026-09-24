package profile

import (
	"fmt"
	"net/url"

	"github.com/kritama/tama-link/internal/catalog"
)

// Kind is the single execution model a profile may own. One process serves
// one profile, and one profile is either the App task model or the System
// replay model, never both.
type Kind string

const (
	// KindApp owns owner-bound upstream tasks. Its endpoint is /mcp/app.
	KindApp Kind = "app"
	// KindSystem owns local replayable execution. Its endpoint is /mcp/system.
	KindSystem Kind = "system"
)

// Kind derives the profile's execution model from its operations and
// endpoint. A catalog that mixes App-task and System strategies is rejected.
func (p *Profile) Kind() (Kind, error) {
	if p == nil {
		return "", fmt.Errorf("profile is required")
	}
	kind, err := kindFromOperations(p.Operations)
	if err != nil {
		return "", err
	}
	endpoint, err := url.Parse(p.Endpoint)
	if err != nil {
		return "", fmt.Errorf("endpoint %q is not a URL: %v", p.Endpoint, err)
	}
	if err := checkEndpointKind(endpoint, kind); err != nil {
		return "", err
	}
	return kind, nil
}

func kindFromOperations(ops []catalog.Descriptor) (Kind, error) {
	var app, system bool
	for _, op := range ops {
		switch op.Strategy {
		case catalog.StrategyUpstreamTask:
			app = true
		case catalog.StrategyLocalReplayable, catalog.StrategyLocalGuarded, catalog.StrategyUnsupported:
			system = true
		default:
			return "", fmt.Errorf("operation %q has unknown strategy %q", op.Name, op.Strategy)
		}
	}
	switch {
	case app && system:
		return "", fmt.Errorf("profile mixes app and system operations")
	case app:
		return KindApp, nil
	case system:
		return KindSystem, nil
	default:
		return "", fmt.Errorf("profile has no operations")
	}
}

func checkEndpointKind(endpoint *url.URL, kind Kind) error {
	switch kind {
	case KindApp:
		if endpoint.Path != "/mcp/app" {
			return fmt.Errorf("app profile endpoint %q must use /mcp/app", endpoint)
		}
	case KindSystem:
		if endpoint.Path != "/mcp/system" {
			return fmt.Errorf("system profile endpoint %q must use /mcp/system", endpoint)
		}
	default:
		return fmt.Errorf("unknown profile kind %q", kind)
	}
	return nil
}
