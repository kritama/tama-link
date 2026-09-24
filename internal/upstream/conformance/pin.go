package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Protocol version and immutable TamaMCP fixture pins.
//
// Core and Tasks documents are the specification baseline. They are unchanged
// through the subscription release. Subscription fixtures did not exist at
// that baseline; they are pinned to the v0.2.0 release that published them.
// The protocol version is still 2026-07-28, so profile bounds are not
// regenerated for this pin.
const (
	ProtocolVersion = "2026-07-28"

	CoreTasksCommit     = "6b5db00018d2774834db5a0f00eed5b9b55e1d2e"
	SubscriptionCommit  = "5c80c29e90c49438fbcc331db5c00f9f8f93ee21"
	SubscriptionRelease = "v0.2.0"

	coreSHA256          = "9beb92b9d7b11c205e47656e071e740868a5a9481fa440b62ee8abd572eab19e"
	tasksSHA256         = "5ddf8b26171715ba60af946899f6e1f2c9adfb049fb49a5a0ea603d23889b6b1"
	subscriptionsSHA256 = "f5b59c430764d8fd7f1e5d905eed358a1922a44b4aef2b55f12200af8347e190"

	coreFixtureCount         = 16
	tasksFixtureCount        = 23
	taskSchemaFixtureCount   = 11
	subscriptionFixtureCount = 7
)

// pin identifies one vendored document.
type pin struct {
	name   string
	commit string
	sha256 string
	body   []byte
}

func pins() []pin {
	return []pin{
		{name: "core.json", commit: CoreTasksCommit, sha256: coreSHA256, body: coreJSON},
		{name: "tasks.json", commit: CoreTasksCommit, sha256: tasksSHA256, body: tasksJSON},
		{name: "subscriptions.json", commit: SubscriptionCommit, sha256: subscriptionsSHA256, body: subscriptionsJSON},
	}
}

// verifyPins fails when a vendored document drifts from its immutable pin.
func verifyPins() error {
	for _, item := range pins() {
		sum := sha256.Sum256(item.body)
		got := hex.EncodeToString(sum[:])
		if got != item.sha256 {
			return fmt.Errorf("%s sha256 %s, pin %s at %s", item.name, got, item.sha256, item.commit)
		}
	}
	return nil
}
