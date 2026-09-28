package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/oauth"
	"github.com/kritama/tama-link/internal/profile"
)

type candidate struct {
	Name     profile.Name
	Origin   string
	Endpoint string
	Issuer   string
	Template tama2026.Template
}

func (s *Service) create(ctx context.Context, req Request) (*Result, error) {
	cand, err := s.collect(ctx, req)
	if err != nil {
		return nil, err
	}
	if existing, err := s.journalFor(cand.Name); err != nil {
		return nil, err
	} else if existing.Name != "" {
		return s.resume(ctx, req, existing)
	}
	if err := s.confirm(ctx, req, cand); err != nil {
		return nil, err
	}
	record, err := s.reserve(cand)
	if err != nil {
		return nil, err
	}
	return s.finish(ctx, req, cand, record)
}

func (s *Service) collect(ctx context.Context, req Request) (candidate, error) {
	address, err := s.address(ctx, req)
	if err != nil {
		return candidate{}, err
	}
	kind, err := s.kind(ctx, req)
	if err != nil {
		return candidate{}, err
	}
	origin, err := profile.ParseOrigin(address)
	if err != nil {
		return candidate{}, usageErr(err)
	}
	endpoint, err := profile.EndpointFor(origin, kind)
	if err != nil {
		return candidate{}, usageErr(err)
	}
	name, err := s.profileName(ctx, req, kind)
	if err != nil {
		return candidate{}, err
	}
	tmpl, err := tama2026.Lookup(kind)
	if err != nil {
		return candidate{}, failErr(err)
	}
	issuer, err := s.issuer(ctx, req, endpoint, tmpl.Scopes)
	if err != nil {
		return candidate{}, err
	}
	return candidate{Name: name, Origin: origin, Endpoint: endpoint, Issuer: issuer, Template: tmpl}, nil
}

func (s *Service) address(ctx context.Context, req Request) (string, error) {
	if req.AddressSet {
		return req.Address, nil
	}
	return s.ask.ask(ctx, "Tama address", "")
}

func (s *Service) kind(ctx context.Context, req Request) (profile.Kind, error) {
	value := req.Type
	if !req.TypeSet {
		if !s.opts.Interactive {
			value = string(profile.KindApp)
		} else {
			got, err := s.ask.ask(ctx, "Type", string(profile.KindApp))
			if err != nil {
				return "", err
			}
			value = got
		}
	}
	kind, err := profile.ParseKind(value)
	if err != nil {
		return "", usageErr(err)
	}
	return kind, nil
}

func (s *Service) profileName(ctx context.Context, req Request, kind profile.Kind) (profile.Name, error) {
	value := req.Profile
	if !req.ProfileSet {
		value = tama2026.DefaultName(kind)
		if !s.opts.Interactive {
			name, err := profile.ParseName(value)
			if err != nil {
				return "", usageErr(err)
			}
			if err := s.rejectExisting(name); err != nil {
				return "", err
			}
			return name, nil
		}
		got, err := s.ask.ask(ctx, "Profile name", value)
		if err != nil {
			return "", err
		}
		value = got
	}
	name, err := profile.ParseName(value)
	if err != nil {
		return "", usageErr(err)
	}
	if err := s.rejectExisting(name); err != nil {
		if !s.opts.Interactive || req.ProfileSet {
			return "", err
		}
		s.ask.note("profile %q already exists", name)
		got, err := s.ask.ask(ctx, "Profile name", "")
		if err != nil {
			return "", err
		}
		name, err = profile.ParseName(got)
		if err != nil {
			return "", usageErr(err)
		}
		if err := s.rejectExisting(name); err != nil {
			return "", err
		}
	}
	return name, nil
}

func (s *Service) rejectExisting(name profile.Name) error {
	_, err := profile.Load(name, s.opts.ConfigDir)
	if errors.Is(err, profile.ErrNotFound) {
		return nil
	}
	if err != nil {
		return usageErr(err)
	}
	return usageErr(fmt.Errorf("%w: %s", profile.ErrExists, name))
}

func (s *Service) issuer(ctx context.Context, req Request, endpoint string, scopes []string) (string, error) {
	prm, err := oauth.DiscoverProtectedResource(ctx, endpoint, s.opts.HTTP)
	if err != nil {
		return "", failErr(fmt.Errorf("discover protected-resource metadata: %v", err))
	}
	candidates, err := oauth.ValidAuthorizationServers(prm)
	if err != nil {
		return "", failErr(err)
	}
	selected, err := s.selectIssuer(ctx, req, candidates)
	if err != nil {
		return "", err
	}
	as, err := oauth.DiscoverAuthorizationServer(ctx, selected, s.opts.HTTP)
	if err != nil {
		return "", failErr(fmt.Errorf("discover authorization-server metadata: %v", err))
	}
	md := &oauth.Metadata{PRM: *prm, AS: *as, ASURL: selected}
	if err := md.CheckRequestedScopes(scopes); err != nil {
		return "", failErr(fmt.Errorf("template scopes are not advertised: %v", err))
	}
	return as.Issuer, nil
}

func (s *Service) selectIssuer(ctx context.Context, req Request, candidates []string) (string, error) {
	if req.IssuerSet {
		for _, candidate := range candidates {
			if sameIssuer(candidate, req.Issuer) {
				return candidate, nil
			}
		}
		return "", usageErr(fmt.Errorf("issuer is not advertised by the protected resource"))
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	if !s.opts.Interactive {
		return "", usageErr(fmt.Errorf("authorization server selection is ambiguous; pass --issuer"))
	}
	index, err := s.ask.choose(ctx, "Authorization server:", candidates)
	if err != nil {
		return "", err
	}
	return candidates[index], nil
}

func (s *Service) confirm(ctx context.Context, req Request, cand candidate) error {
	if !s.opts.Interactive || req.Yes {
		return nil
	}
	s.ask.note("endpoint %s", cand.Endpoint)
	s.ask.note("authorization server %s", cand.Issuer)
	s.ask.note("profile %s", cand.Name)
	s.ask.note("access %s", cand.Template.Label)
	label := "Continue in your browser?"
	if req.NoBrowser {
		label = "Continue with manual authorization?"
	}
	ok, err := s.ask.confirm(ctx, label, true)
	if err != nil {
		return err
	}
	if !ok {
		return cancelled()
	}
	return nil
}

func (s *Service) reserve(cand candidate) (journal, error) {
	id, err := newJournalID()
	if err != nil {
		return journal{}, failErr(err)
	}
	record := journal{
		Version: journalVersion, ID: id, Name: cand.Name.String(),
		Origin: cand.Origin, Endpoint: cand.Endpoint, Issuer: cand.Issuer,
		Template: cand.Template.ID, Scopes: cand.Template.Scopes,
		Database: stateDatabase, Credentials: stateCredentials, Stage: stageReserved,
	}
	if err := record.create(s.opts.ConfigDir); err != nil {
		return journal{}, err
	}
	return record, nil
}

func shellProfile(cand candidate) *profile.Profile {
	return &profile.Profile{
		Version: profile.SchemaVersion, Name: cand.Name,
		Origin: cand.Origin, Endpoint: cand.Endpoint, Issuer: cand.Issuer,
		Instructions: cand.Template.Instructions, Bounds: cand.Template.Bounds,
		State:  profile.StateRefs{Database: stateDatabase, Credentials: stateCredentials},
		Scopes: cand.Template.Scopes,
	}
}

func completeProfile(shell *profile.Profile, ops []catalog.Descriptor) (*profile.Profile, error) {
	full := *shell
	full.Operations = ops
	digest, err := full.ComputeDigest()
	if err != nil {
		return nil, err
	}
	full.Digest = digest
	if err := full.Validate(full.Name); err != nil {
		return nil, err
	}
	return &full, nil
}

func sameIssuer(left, right string) bool {
	return strings.TrimSuffix(left, "/") == strings.TrimSuffix(right, "/")
}

func serveCommand(name profile.Name, configDir string, set bool) string {
	cmd := "tama-link serve --profile " + name.String()
	if set && configDir != "" {
		cmd += " --config-dir " + quoteCommandArg(configDir)
	}
	return cmd
}

// quoteCommandArg quotes one command argument for copy-paste. It preserves
// backslashes: Go's strconv.Quote would double them, and Windows shells would
// then keep both.
func quoteCommandArg(value string) string {
	if runtime.GOOS == "windows" {
		return quoteWindowsArg(value)
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func quoteWindowsArg(value string) string {
	if value == "" {
		return `""`
	}
	if !strings.ContainsAny(value, " \t\"") {
		return value
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\':
			slashes++
		case '"':
			for range slashes*2 + 1 {
				b.WriteByte('\\')
			}
			b.WriteByte('"')
			slashes = 0
		default:
			for range slashes {
				b.WriteByte('\\')
			}
			slashes = 0
			b.WriteByte(value[i])
		}
	}
	for range slashes * 2 {
		b.WriteByte('\\')
	}
	b.WriteByte('"')
	return b.String()
}
