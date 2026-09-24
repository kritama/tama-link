package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Compose service names the pin check requires. A file that does not name
// these services is not the migrated topology, whatever revisions the
// environment claims.
const (
	ServiceTama     = "tama"
	ServiceTamaMCP  = "tama-mcp"
	ServiceProvider = "provider"
)

// ComposeRevisions are the reported TamaMCP, Tama, and provider pins. Each
// must be the service image tag, image digest, or remote build ref.
type ComposeRevisions struct {
	TamaMCP  string
	Tama     string
	Provider string
}

// composeService is one resolved service. BuildRef is only a git fragment
// from a remote build source, never a local context or build argument.
type composeService struct {
	Image      string
	BuildRef   string
	PullPolicy string
	HasBuild   bool
}

// floatingTags are image tags that do not identify one immutable revision.
var floatingTags = map[string]bool{
	"latest": true, "stable": true, "edge": true, "nightly": true,
	"main": true, "master": true, "develop": true, "dev": true,
	"rolling": true, "canary": true,
}

// CheckComposePins resolves the Compose model with `docker compose config`
// and proves the expected services pin the reported revisions. It does not
// start containers. A raw file read is not treated as resolved configuration.
func CheckComposePins(path string, revisions ComposeRevisions) error {
	resolved, err := dockerComposeConfig(path)
	if err != nil {
		return fmt.Errorf("compose model could not be resolved: %w", err)
	}
	services, err := parseComposeJSON(resolved)
	if err != nil {
		return err
	}
	return checkServices(services, revisions)
}

func checkServices(services map[string]composeService, revisions ComposeRevisions) error {
	if !RevisionOK(revisions.TamaMCP) || !RevisionOK(revisions.Tama) || !RevisionOK(revisions.Provider) {
		return fmt.Errorf("compose pins require TamaMCP, Tama, and provider revisions")
	}
	required := []struct {
		name     string
		revision string
	}{
		{ServiceTamaMCP, revisions.TamaMCP},
		{ServiceTama, revisions.Tama},
		{ServiceProvider, revisions.Provider},
	}
	var missing []string
	for _, item := range required {
		service, ok := services[item.name]
		if !ok {
			missing = append(missing, item.name)
			continue
		}
		if err := servicePinned(item.name, service, item.revision); err != nil {
			return err
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("compose file is missing services %s", strings.Join(missing, ", "))
	}
	return nil
}

func dockerComposeConfig(path string) ([]byte, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("docker is required to resolve the compose model: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", path, "config", "--format", "json")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, composeCommandError(ctx, stderr.String(), err)
	}
	return out, nil
}

func composeCommandError(ctx context.Context, stderr string, err error) error {
	detail := strings.TrimSpace(stderr)
	if ctxErr := ctx.Err(); ctxErr != nil {
		if detail == "" {
			return fmt.Errorf("docker compose config: %w: %w", ctxErr, err)
		}
		return fmt.Errorf("docker compose config: %s: %w: %w", detail, ctxErr, err)
	}
	if detail == "" {
		return fmt.Errorf("docker compose config: %w", err)
	}
	return fmt.Errorf("docker compose config: %s: %w", detail, err)
}

func parseComposeJSON(body []byte) (map[string]composeService, error) {
	var doc struct {
		Services map[string]struct {
			Image      string          `json:"image"`
			Build      json.RawMessage `json:"build"`
			PullPolicy string          `json:"pull_policy"`
		} `json:"services"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse resolved compose model: %w", err)
	}
	out := make(map[string]composeService, len(doc.Services))
	for name, service := range doc.Services {
		ref, hasBuild, err := buildFromJSON(service.Build)
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", name, err)
		}
		out[name] = composeService{
			Image:      service.Image,
			BuildRef:   ref,
			PullPolicy: service.PullPolicy,
			HasBuild:   hasBuild,
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("resolved compose model has no services")
	}
	return out, nil
}

func buildFromJSON(raw json.RawMessage) (string, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
		return "", false, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false, err
	}
	return immutableBuildRef(value), true, nil
}

// immutableBuildRef returns the git ref of a remote build source. A local
// context, dockerfile path, argument, label, or other metadata is not a ref.
func immutableBuildRef(build any) string {
	switch value := build.(type) {
	case string:
		return gitRef(value)
	case map[string]any:
		context, _ := value["context"].(string)
		return gitRef(context)
	default:
		return ""
	}
}

func gitRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || ref == "." || strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "../") || strings.HasPrefix(ref, "/") {
		return ""
	}
	hash := strings.LastIndex(ref, "#")
	if hash < 0 || hash == len(ref)-1 {
		return ""
	}
	fragment := ref[hash+1:]
	if colon := strings.Index(fragment, ":"); colon >= 0 {
		fragment = fragment[:colon]
	}
	if strings.Contains(fragment, "/") || strings.Contains(ref[:hash], "://") || strings.Contains(ref[:hash], "@") {
		return fragment
	}
	return ""
}

func servicePinned(name string, service composeService, revision string) error {
	hasImage := strings.TrimSpace(service.Image) != ""
	tag, digest, tagged := imageRef(service.Image)
	if hasImage && !tagged {
		return fmt.Errorf("service %s image %q has no tag or digest", name, service.Image)
	}
	imagePinned := imagePins(tag, digest, revision)
	buildPinned := service.HasBuild && service.BuildRef == revision
	buildSelected := strings.EqualFold(strings.TrimSpace(service.PullPolicy), "build")

	if hasImage && service.HasBuild {
		return combinedServicePinned(name, revision, imagePinned, buildPinned, buildSelected)
	}
	if imagePinned || buildPinned {
		return nil
	}
	if hasImage && tagged && digest == "" && floatingTags[strings.ToLower(tag)] {
		return fmt.Errorf("service %s image tag %q is mutable", name, tag)
	}
	return fmt.Errorf("service %s does not pin revision %s as an image tag, digest, or remote build ref", name, revision)
}

func combinedServicePinned(
	name string,
	revision string,
	imagePinned bool,
	buildPinned bool,
	buildSelected bool,
) error {
	if buildSelected {
		if buildPinned {
			return nil
		}
		return fmt.Errorf("service %s selects an unpinned build source with pull_policy build", name)
	}
	if imagePinned && buildPinned {
		return nil
	}
	return fmt.Errorf(
		"service %s may select an unpinned image or build source for revision %s; pin both or use pull_policy build with a pinned build ref",
		name,
		revision,
	)
}

func imagePins(tag, digest, revision string) bool {
	if digest != "" {
		hex := strings.TrimPrefix(digest, "sha256:")
		return digest == revision || hex == revision
	}
	return tag != "" && tag == revision
}

func imageRef(image string) (tag, digest string, ok bool) {
	image = strings.TrimSpace(image)
	if image == "" {
		return "", "", false
	}
	if at := strings.LastIndex(image, "@"); at >= 0 {
		digest = image[at+1:]
		return "", digest, digest != ""
	}
	name := image
	if slash := strings.LastIndex(image, "/"); slash >= 0 {
		name = image[slash+1:]
	}
	colon := strings.LastIndex(name, ":")
	if colon < 0 || colon == len(name)-1 {
		return "", "", false
	}
	return name[colon+1:], "", true
}
