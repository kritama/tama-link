package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/kritama/tama-link/internal/profile"
)

// Service runs one login invocation, including existing-profile dispatch and
// new-profile bootstrap.
type Service struct {
	opts Options
	ask  prompter
}

// New validates opts and builds a Service.
func New(opts Options) (*Service, error) {
	if opts.OpenSession == nil {
		return nil, errors.New("bootstrap: session opener is required")
	}
	if opts.ReadCatalog == nil {
		return nil, errors.New("bootstrap: catalog reader is required")
	}
	if opts.LoginExisting == nil {
		return nil, errors.New("bootstrap: existing login is required")
	}
	if opts.Stderr == nil {
		return nil, errors.New("bootstrap: stderr is required")
	}
	return &Service{opts: opts, ask: newPrompter(opts.Stdin, opts.Stderr, opts.Interactive)}, nil
}

// Run executes the login invocation described by req.
func (s *Service) Run(ctx context.Context, req Request) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, cancelled()
	}
	if result, done, err := s.recoverOutstanding(ctx, req); err != nil || done {
		return result, err
	}
	if req.ProfileSet {
		return s.runNamed(ctx, req)
	}
	if !s.opts.Interactive {
		if !req.AddressSet {
			return nil, usageErr(fmt.Errorf("login requires --profile to authorize an existing profile, or --address to create one, when input is not a terminal"))
		}
		return s.create(ctx, req)
	}
	return s.runMenu(ctx, req)
}

func (s *Service) runNamed(ctx context.Context, req Request) (*Result, error) {
	name, err := profile.ParseName(req.Profile)
	if err != nil {
		return nil, usageErr(err)
	}
	loaded, err := profile.Load(name, s.opts.ConfigDir)
	if err == nil {
		if req.AddressSet || req.TypeSet || req.IssuerSet || req.Yes {
			return nil, usageErr(fmt.Errorf("existing profile %q already owns its address, type, and issuer", name))
		}
		if err := s.opts.LoginExisting(ctx, loaded); err != nil {
			return nil, err
		}
		return &Result{Name: name}, nil
	}
	if !errors.Is(err, profile.ErrNotFound) {
		return nil, usageErr(err)
	}
	if !s.opts.Interactive && !req.AddressSet {
		return nil, usageErr(err)
	}
	if s.opts.Interactive && !req.AddressSet {
		ok, err := s.ask.confirm(ctx, fmt.Sprintf("Profile %q does not exist. Create it?", name), false)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, cancelled()
		}
	}
	req.Profile = name.String()
	req.ProfileSet = true
	return s.create(ctx, req)
}

func (s *Service) runMenu(ctx context.Context, req Request) (*Result, error) {
	entries, err := profile.List(s.opts.ConfigDir)
	if err != nil {
		return nil, failErr(err)
	}
	var valid []profile.Entry
	for _, entry := range entries {
		if !entry.Valid() {
			s.ask.note("skipping unreadable profile %s", entryProblem(entry))
			continue
		}
		valid = append(valid, entry)
	}
	if len(valid) == 0 || req.AddressSet {
		return s.create(ctx, req)
	}
	options := make([]string, 0, len(valid)+1)
	for _, entry := range valid {
		options = append(options, "log in "+entry.Name.String())
	}
	options = append(options, "add another Tama instance")
	index, err := s.ask.choose(ctx, "Profiles:", options)
	if err != nil {
		return nil, err
	}
	if index == len(valid) {
		return s.create(ctx, req)
	}
	if err := s.opts.LoginExisting(ctx, valid[index].Profile); err != nil {
		return nil, err
	}
	return &Result{Name: valid[index].Name}, nil
}

func entryProblem(entry profile.Entry) string {
	if entry.Name != "" {
		return strconv.Quote(entry.Name.String())
	}
	return "file"
}
