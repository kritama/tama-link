package conformance

import (
	"encoding/json"
	"fmt"

	_ "embed"
)

//go:embed testdata/2026-07-28/core.json
var coreJSON []byte

//go:embed testdata/2026-07-28/tasks.json
var tasksJSON []byte

//go:embed testdata/2026-07-28/subscriptions.json
var subscriptionsJSON []byte

// Set is the pinned core, task, and subscription fixture collection.
type Set struct {
	Core          []Fixture
	Tasks         []Fixture
	Subscriptions []Fixture
	Schema        []SchemaFixture
}

// Wire returns the HTTP fixtures in pin order.
func (s Set) Wire() []Fixture {
	out := make([]Fixture, 0, len(s.Core)+len(s.Tasks)+len(s.Subscriptions))
	out = append(out, s.Core...)
	out = append(out, s.Tasks...)
	out = append(out, s.Subscriptions...)
	return out
}

// Fixture is one package HTTP contract case.
type Fixture struct {
	Name         string   `json:"name"`
	RequestValid bool     `json:"requestValid"`
	Request      Request  `json:"request"`
	Expected     Expected `json:"expected"`
}

// Request is the package's recorded HTTP request. Link adapts it; it does
// not copy the body onto the downstream contract.
type Request struct {
	Headers [][]string      `json:"headers"`
	Body    json.RawMessage `json:"body"`
}

// Expected is the package's recorded HTTP response.
type Expected struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
	Events  []json.RawMessage `json:"events"`
	Close   string            `json:"close"`
}

// SchemaFixture is one static positive or negative task value.
type SchemaFixture struct {
	Name   string          `json:"name"`
	Schema string          `json:"schema"`
	Valid  bool            `json:"valid"`
	Value  json.RawMessage `json:"value"`
}

type fixtureDocument struct {
	Fixtures       []Fixture       `json:"fixtures"`
	SchemaFixtures []SchemaFixture `json:"schemaFixtures"`
}

// loadSet reads and checks the vendored documents.
func loadSet() (Set, error) {
	if err := verifyPins(); err != nil {
		return Set{}, err
	}
	var core, tasks, subscriptions fixtureDocument
	if err := json.Unmarshal(coreJSON, &core); err != nil {
		return Set{}, fmt.Errorf("decode core fixtures: %w", err)
	}
	if err := json.Unmarshal(tasksJSON, &tasks); err != nil {
		return Set{}, fmt.Errorf("decode task fixtures: %w", err)
	}
	if err := json.Unmarshal(subscriptionsJSON, &subscriptions); err != nil {
		return Set{}, fmt.Errorf("decode subscription fixtures: %w", err)
	}
	set := Set{
		Core:          core.Fixtures,
		Tasks:         tasks.Fixtures,
		Subscriptions: subscriptions.Fixtures,
		Schema:        tasks.SchemaFixtures,
	}
	if len(set.Core) != coreFixtureCount || len(set.Tasks) != tasksFixtureCount ||
		len(set.Schema) != taskSchemaFixtureCount || len(set.Subscriptions) != subscriptionFixtureCount {
		return Set{}, fmt.Errorf("fixture counts core=%d tasks=%d schema=%d subscriptions=%d",
			len(set.Core), len(set.Tasks), len(set.Schema), len(set.Subscriptions))
	}
	return set, nil
}

type rpcBody struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcParams struct {
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments"`
	TaskID         string          `json:"taskId"`
	InputResponses json.RawMessage `json:"inputResponses"`
	Notifications  rpcNotes        `json:"notifications"`
	Meta           rpcMeta         `json:"_meta"`
	Task           json.RawMessage `json:"task"`
}

type rpcNotes struct {
	TaskIDs []string `json:"taskIds"`
}

type rpcMeta struct {
	Capabilities json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
}

// Method returns the fixture request method.
func (fx Fixture) Method() string {
	body, err := fx.rpc()
	if err != nil {
		return ""
	}
	return body.Method
}

// RequestID returns the fixture's JSON-RPC id.
func (fx Fixture) RequestID() string {
	body, err := fx.rpc()
	if err != nil {
		return ""
	}
	return body.ID
}

func (fx Fixture) rpc() (rpcBody, error) {
	var body rpcBody
	err := json.Unmarshal(fx.Request.Body, &body)
	return body, err
}

func (fx Fixture) params() (rpcParams, error) {
	body, err := fx.rpc()
	if err != nil {
		return rpcParams{}, err
	}
	var params rpcParams
	if len(body.Params) == 0 {
		return params, nil
	}
	err = json.Unmarshal(body.Params, &params)
	return params, err
}

// legacy reports whether the fixture request is a removed method Link must
// not emit.
func (fx Fixture) legacy() bool {
	switch fx.Method() {
	case "initialize", "notifications/initialized", "tasks/result", "tasks/list":
		return true
	default:
		return false
	}
}

// declaresTasks reports whether the fixture request declares the Tasks extension.
func (fx Fixture) declaresTasks() bool {
	params, err := fx.params()
	if err != nil || !jsonObject(params.Meta.Capabilities) {
		return false
	}
	var caps struct {
		Extensions map[string]json.RawMessage `json:"extensions"`
	}
	if json.Unmarshal(params.Meta.Capabilities, &caps) != nil {
		return false
	}
	ext, ok := caps.Extensions["io.modelcontextprotocol/tasks"]
	return ok && jsonObject(ext)
}

func jsonObject(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var v map[string]json.RawMessage
	return json.Unmarshal(raw, &v) == nil
}
