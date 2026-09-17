package credential

import (
	"fmt"
	"time"

	"github.com/99designs/keyring"
)

// probeRunner executes availability probes through one owned worker
// goroutine. The keyring API takes no context, so an in-flight call
// cannot be interrupted; the runner bounds that by keeping exactly one
// worker alive instead of abandoning a goroutine per probe, and by
// confining every probe to its own unguessable key, so a call that
// outlives its deadline can only ever finish its own Set/Get/Remove
// cycle on one disposable entry.
type probeRunner struct {
	requests chan probeRequest
}

// probeRequest carries one probe to the worker; done is buffered so the
// worker never blocks on a caller that already gave up on the deadline.
type probeRequest struct {
	kr   keyring.Keyring
	key  string
	done chan error
}

func newProbeRunner() *probeRunner {
	r := &probeRunner{requests: make(chan probeRequest)}
	go r.run()
	return r
}

// run is the single probe worker. It exits when its owner closes the
// request channel; a request already executing a blocking keyring call
// finishes that call first — it cannot be interrupted — and then observes
// the closed channel and returns, so the worker always has a shutdown
// path even though no individual call can be canceled.
func (r *probeRunner) run() {
	for req := range r.requests {
		req.done <- runProbe(req.kr, req.key)
	}
}

// probe runs one availability probe and bounds it with probeTimeout. On
// timeout the request is abandoned, not the worker: the worker stays
// owned by the runner and serves the next probe once the blocking call
// returns.
func (r *probeRunner) probe(kr keyring.Keyring, key string) error {
	req := probeRequest{kr: kr, key: key, done: make(chan error, 1)}
	select {
	case r.requests <- req:
	case <-time.After(probeTimeout):
		return fmt.Errorf("credential backend did not accept an availability probe within %s; a keyring unlock prompt is not a supported serve-startup path", probeTimeout)
	}
	select {
	case err := <-req.done:
		return err
	case <-time.After(probeTimeout):
		return fmt.Errorf("credential backend did not complete an availability probe within %s; a keyring unlock prompt is not a supported serve-startup path", probeTimeout)
	}
}

// close shuts the runner down: no further probes are accepted and the
// worker exits after any in-flight call returns. It deliberately does not
// wait for the worker — joining an uninterruptible keyring call would
// reintroduce the unbounded startup delay the probe timeout exists to
// prevent.
func (r *probeRunner) close() {
	close(r.requests)
}

// runProbe performs the Set/Get/Remove availability cycle on one
// disposable entry.
func runProbe(kr keyring.Keyring, key string) error {
	if err := kr.Set(keyring.Item{
		Key:         key,
		Data:        []byte{1},
		Label:       "Tama Link availability probe",
		Description: "Temporary entry; safe to delete",
	}); err != nil {
		return err
	}
	if _, err := kr.Get(key); err != nil {
		return err
	}
	return kr.Remove(key)
}
