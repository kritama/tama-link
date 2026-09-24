package acceptance

import "testing"

func TestComposePinsRejectUnrelatedTopology(t *testing.T) {
	err := checkComposeSource(t, "services:\n  unrelated:\n    image: busybox\n")
	if err == nil {
		t.Fatal("unrelated unpinned topology accepted")
	}
}

func TestComposePinsRejectImplicitLatestAndUnrelatedRevision(t *testing.T) {
	err := checkComposeSource(t, `
services:
  tama:
    image: ghcr.io/example/tama:latest
  tama-mcp:
    image: ghcr.io/example/tama-mcp:abcdef0
  provider:
    image: ghcr.io/example/provider:abcdef2
`)
	if err == nil {
		t.Fatal("latest image accepted")
	}
}

func TestComposePinsRejectMutableTagWithUnrelatedBuildArg(t *testing.T) {
	err := checkComposeSource(t, `
services:
  tama:
    image: ghcr.io/example/tama:stable
    build:
      context: .
      args:
        UNRELATED: abcdef1
  tama-mcp:
    image: ghcr.io/example/tama-mcp:stable
    build:
      context: .
      args:
        UNRELATED: abcdef0
  provider:
    image: ghcr.io/example/provider:stable
    build:
      context: .
      args:
        UNRELATED: abcdef2
`)
	if err == nil {
		t.Fatal("mutable :stable image accepted via unrelated build arg")
	}
	resolved := []byte(`{
	  "services": {
	    "tama": {"image": "ghcr.io/example/tama:stable", "build": {"context": ".", "args": {"UNRELATED": "abcdef1"}}},
	    "tama-mcp": {"image": "ghcr.io/example/tama-mcp:stable", "build": {"context": ".", "args": {"UNRELATED": "abcdef0"}}},
	    "provider": {"image": "ghcr.io/example/provider:stable", "build": {"context": ".", "args": {"UNRELATED": "abcdef2"}}}
	  }
	}`)
	services, err := parseComposeJSON(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkServices(services, reviewRevisions()); err == nil {
		t.Fatal("resolved mutable :stable model accepted via unrelated build arg")
	}
}

func TestComposePinsRespectImageBuildSelection(t *testing.T) {
	tests := []struct {
		name    string
		service composeService
		wantErr bool
	}{
		{
			name: "default pull may bypass pinned build",
			service: composeService{
				Image:    "ghcr.io/example/tama:stable",
				BuildRef: "abcdef1",
				HasBuild: true,
			},
			wantErr: true,
		},
		{
			name: "build policy selects pinned build",
			service: composeService{
				Image:      "ghcr.io/example/tama:stable",
				BuildRef:   "abcdef1",
				PullPolicy: "build",
				HasBuild:   true,
			},
		},
		{
			name: "build policy rejects local build despite pinned image",
			service: composeService{
				Image:      "ghcr.io/example/tama:abcdef1",
				PullPolicy: "build",
				HasBuild:   true,
			},
			wantErr: true,
		},
		{
			name: "default fallback rejects local build despite pinned image",
			service: composeService{
				Image:    "ghcr.io/example/tama:abcdef1",
				HasBuild: true,
			},
			wantErr: true,
		},
		{
			name: "both possible sources pin the revision",
			service: composeService{
				Image:    "ghcr.io/example/tama:abcdef1",
				BuildRef: "abcdef1",
				HasBuild: true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			services := map[string]composeService{
				ServiceTama:     tt.service,
				ServiceTamaMCP:  {Image: "ghcr.io/example/tama-mcp:abcdef0"},
				ServiceProvider: {Image: "ghcr.io/example/provider:abcdef2"},
			}
			err := checkServices(services, reviewRevisions())
			if tt.wantErr && err == nil {
				t.Fatal("unpinned runtime source accepted")
			}
			if !tt.wantErr && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParseComposeJSONPreservesBuildSelection(t *testing.T) {
	services, err := parseComposeJSON([]byte(`{
	  "services": {
	    "tama": {
	      "image": "ghcr.io/example/tama:stable",
	      "pull_policy": "build",
	      "build": {"context": "https://github.com/example/tama.git#abcdef1"}
	    }
	  }
	}`))
	if err != nil {
		t.Fatal(err)
	}
	got := services[ServiceTama]
	if !got.HasBuild || got.PullPolicy != "build" || got.BuildRef != "abcdef1" {
		t.Fatalf("resolved build selection = %#v", got)
	}
}

func TestComposePinsRequireImmutableImageOrBuildRef(t *testing.T) {
	err := checkComposeSource(t, `
services:
  tama:
    image: ghcr.io/example/tama:abcdef1
  tama-mcp:
    image: ghcr.io/example/tama-mcp:abcdef0
  provider:
    build: ./provider
`)
	if err == nil {
		t.Fatal("local build context accepted as a revision")
	}
	if err := checkComposeSource(t, `
services:
  tama:
    image: ghcr.io/example/tama:abcdef1
  tama-mcp:
    build:
      context: https://github.com/example/tama-mcp.git#abcdef0
  provider:
    image: ghcr.io/example/provider@sha256:abcdef2
`); err != nil {
		t.Fatal(err)
	}
}

func TestComposePinsIgnoreRevisionInsideRepositoryPath(t *testing.T) {
	err := checkComposeSource(t, `
services:
  tama:
    image: ghcr.io/example/tama:stable
  tama-mcp:
    build:
      context: https://github.com/example/abcdef0.git#main
  provider:
    image: ghcr.io/example/provider:abcdef2
`)
	if err == nil {
		t.Fatal("revision in a repository path accepted without an immutable ref")
	}
}

func checkComposeSource(t *testing.T, body string) error {
	t.Helper()
	services, err := parseComposeYAML([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return checkServices(services, reviewRevisions())
}

func reviewRevisions() ComposeRevisions {
	return ComposeRevisions{TamaMCP: "abcdef0", Tama: "abcdef1", Provider: "abcdef2"}
}
