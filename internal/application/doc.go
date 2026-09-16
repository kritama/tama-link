// Package application is Tama Link's application service for one process
// profile: it validates and accepts downstream submit calls, dispatches the
// App task workflow and the leased System workflow by pinned execution
// strategy, and serves await long-polls from durable local state.
//
// The package orchestrates the store, the verified upstream adapter, and the
// leased worker. It contains no transport, no OAuth, no catalog discovery,
// and no scheduling policy beyond one bounded long-poll per await call.
package application
