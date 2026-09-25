package login

// handoff presents the authorization URL to the user. With an opener, the
// fixed platform browser opens it; a launch failure or a nil opener falls
// back to the manual handoff. The fallback never invalidates the attempt:
// the bounded callback wait continues unchanged.
func (s *Service) handoff(rawURL string) {
	if s.openBrowser != nil {
		if err := s.openBrowser(rawURL); err == nil {
			s.reporter.Note("waiting for browser authorization (up to %s)", s.callbackWait)
			return
		}
		s.reporter.Note("could not open a browser; open the authorization URL in your browser")
	}
	s.reporter.AuthorizationURL(rawURL)
	s.reporter.Note("waiting for the authorization callback (up to %s)", s.callbackWait)
}
