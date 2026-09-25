package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/profile"
)

func timeAfter(d time.Duration) <-chan time.Time { return time.After(d) }

func (s *Service) journalFor(name profile.Name) (journal, error) {
	record, err := loadJournal(s.opts.ConfigDir, name)
	if err != nil {
		if os.IsNotExist(err) {
			return journal{}, nil
		}
		return journal{}, failErr(fmt.Errorf("read bootstrap journal: %w", err))
	}
	return record, nil
}

func (s *Service) recoverOutstanding(ctx context.Context, req Request) (*Result, bool, error) {
	names, err := listJournals(s.opts.ConfigDir)
	if err != nil {
		return nil, false, failErr(err)
	}
	if len(names) == 0 {
		return nil, false, nil
	}
	if !s.opts.Interactive {
		if req.ProfileSet {
			for _, name := range names {
				if name.String() == req.Profile {
					return nil, false, incomplete(fmt.Errorf("profile %q has an unfinished bootstrap; run login interactively to resume or discard it", name))
				}
			}
		}
		return nil, false, nil
	}
	options := make([]string, 0, len(names)*2+1)
	for _, name := range names {
		options = append(options, "resume "+name.String(), "discard "+name.String())
	}
	options = append(options, "continue")
	index, err := s.ask.choose(ctx, "Incomplete bootstrap:", options)
	if err != nil {
		return nil, false, err
	}
	if index == len(options)-1 {
		return nil, false, nil
	}
	name := names[index/2]
	record, err := loadJournal(s.opts.ConfigDir, name)
	if err != nil {
		return nil, false, failErr(err)
	}
	if index%2 == 1 {
		return nil, false, s.discard(ctx, record)
	}
	result, err := s.resume(ctx, req, record)
	return result, true, err
}

func (s *Service) resume(ctx context.Context, req Request, record journal) (*Result, error) {
	if err := contradict(req, record); err != nil {
		return nil, usageErr(err)
	}
	kind := profile.KindApp
	if record.Template == "system-read" {
		kind = profile.KindSystem
	}
	tmpl, err := tama2026.Lookup(kind)
	if err != nil {
		return nil, failErr(err)
	}
	issuer, err := s.issuer(ctx, Request{Issuer: record.Issuer, IssuerSet: true}, record.Endpoint, tmpl.Scopes)
	if err != nil {
		return nil, err
	}
	if issuer != record.Issuer {
		return nil, failErr(fmt.Errorf("advertised issuer no longer matches the bootstrap journal"))
	}
	cand := candidate{
		Name: profile.Name(record.Name), Origin: record.Origin,
		Endpoint: record.Endpoint, Issuer: issuer, Template: tmpl,
	}
	if loaded, err := profile.Load(cand.Name, s.opts.ConfigDir); err == nil {
		return s.completePublished(ctx, cand, loaded)
	}
	return s.finish(ctx, req, cand, record)
}

func (s *Service) completePublished(ctx context.Context, cand candidate, loaded *profile.Profile) (*Result, error) {
	if loaded.Name != cand.Name || loaded.Origin != cand.Origin || loaded.Endpoint != cand.Endpoint || loaded.Issuer != cand.Issuer {
		return nil, failErr(fmt.Errorf("%w: existing profile was preserved", profile.ErrExists))
	}
	session, err := s.opts.OpenSession(ctx, loaded)
	if err != nil {
		return nil, incomplete(err)
	}
	defer func() { _ = session.Close() }()
	ready, err := session.Ready(ctx)
	if err != nil || !ready {
		return nil, incomplete(fmt.Errorf("published profile has no matching credential"))
	}
	token, err := session.Token(ctx)
	if err != nil {
		return nil, incomplete(fmt.Errorf("read access token: %v", err))
	}
	disc, live, err := s.opts.ReadCatalog(ctx, cand.Endpoint, token)
	if err != nil {
		return nil, incomplete(fmt.Errorf("discover catalog: %v", err))
	}
	ops, err := cand.Template.Materialize(disc, live)
	if err != nil {
		return nil, incomplete(err)
	}
	expected, err := completeProfile(shellProfile(cand), ops)
	if err != nil {
		return nil, incomplete(err)
	}
	return s.reconcileWinner(ctx, cand, expected)
}

// reconcileWinner accepts an existing profile only when it is the reviewed
// candidate. A different same-name winner is left unchanged, and the journal
// stays so the unfinished bootstrap remains recoverable.
func (s *Service) reconcileWinner(ctx context.Context, cand candidate, expected *profile.Profile) (*Result, error) {
	loaded, err := profile.Load(cand.Name, s.opts.ConfigDir)
	if err != nil {
		return nil, incomplete(err)
	}
	if !samePublished(loaded, expected) {
		return nil, failErr(fmt.Errorf("%w: existing profile was preserved", profile.ErrExists))
	}
	session, err := s.opts.OpenSession(ctx, loaded)
	if err != nil {
		return nil, incomplete(err)
	}
	defer func() { _ = session.Close() }()
	ready, err := session.Ready(ctx)
	if err != nil || !ready {
		return nil, incomplete(fmt.Errorf("published profile has no matching credential"))
	}
	if err := removeJournal(s.opts.ConfigDir, cand.Name); err != nil {
		return nil, err
	}
	return createdResult(s, cand.Name), nil
}

func samePublished(loaded, expected *profile.Profile) bool {
	if loaded == nil || expected == nil || loaded.Digest == "" || expected.Digest == "" {
		return false
	}
	if loaded.Digest != expected.Digest {
		return false
	}
	got, err := loaded.ComputeDigest()
	return err == nil && got == loaded.Digest
}

func createdResult(s *Service, name profile.Name) *Result {
	return &Result{
		Name:         name,
		Created:      true,
		ServeCommand: serveCommand(name, s.opts.ConfigDir, s.opts.ConfigDirSet),
	}
}

func (s *Service) discard(ctx context.Context, record journal) error {
	return withPublicationLock(s.opts.ConfigDir, func() error {
		return s.discardLocked(ctx, record)
	})
}

func (s *Service) discardLocked(ctx context.Context, record journal) error {
	name := profile.Name(record.Name)
	if _, err := profile.Load(name, s.opts.ConfigDir); err == nil {
		if err := removeJournal(s.opts.ConfigDir, name); err != nil {
			return err
		}
		s.ask.note("preserved existing profile %q and removed the bootstrap journal", name)
		return nil
	} else if !errors.Is(err, profile.ErrNotFound) {
		return failErr(err)
	}
	shell := shellProfile(candidate{
		Name: name, Origin: record.Origin, Endpoint: record.Endpoint, Issuer: record.Issuer,
		Template: tama2026.Template{Instructions: "discard", Bounds: profile.Bounds{ProtocolMin: "2026-07-28", ProtocolMax: "2026-07-28"}, Scopes: record.Scopes},
	})
	session, err := s.opts.OpenSession(ctx, shell)
	if err != nil {
		return incomplete(err)
	}
	defer func() { _ = session.Close() }()
	owner, err := newOwner()
	if err != nil {
		return failErr(err)
	}
	claimed, err := session.ClaimLease(ctx, LeaseName, owner, leaseTTL)
	if err != nil {
		return failErr(err)
	}
	if !claimed {
		return failErr(fmt.Errorf("%w: wait for it to finish and retry", ErrBusy))
	}
	defer func() { _ = session.ReleaseLease(context.WithoutCancel(ctx), LeaseName, owner) }()
	if err := session.Logout(ctx); err != nil {
		return incomplete(err)
	}
	if err := s.removeDatabase(shell); err != nil {
		return incomplete(err)
	}
	return removeJournal(s.opts.ConfigDir, name)
}

func contradict(req Request, record journal) error {
	if req.AddressSet && req.Address != record.Origin {
		return fmt.Errorf("address does not match the unfinished bootstrap")
	}
	if req.IssuerSet && req.Issuer != record.Issuer {
		return fmt.Errorf("issuer does not match the unfinished bootstrap")
	}
	if req.ProfileSet && req.Profile != record.Name {
		return fmt.Errorf("profile name does not match the unfinished bootstrap")
	}
	return nil
}
