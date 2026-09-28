package tama2026

import (
	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/profile"
)

const appInstructions = "Tama MCP app server for submitting messages to configured reasoning recipients. The caller selects the recipient and supplies the public thread identifier; Tama derives the actor from the validated OAuth subject."

func appTemplate() Template {
	thread := objectSchema(false, []string{"identifier"}, map[string]any{
		"identifier": stringField("Stable public thread identifier from the caller; maximum 256 UTF-8 bytes", 1, 256),
	})
	thread["description"] = "Caller-owned conversation identity"
	recipient := stringField("Configured reasoning recipient selected by the caller", 1, 128)
	content := stringField("Message content to submit; maximum 32768 UTF-8 bytes", 1, 0)
	input := objectSchema(false, []string{"recipient", "identifier", "content", "thread"}, map[string]any{
		"recipient":  recipient,
		"identifier": stringField("Stable message identifier from the caller; maximum 256 UTF-8 bytes", 1, 256),
		"content":    content,
		"thread":     thread,
	})
	client := objectSchema(false, []string{"recipient", "content"}, map[string]any{
		"recipient": recipient,
		"content":   content,
	})
	output := variantOutput(
		[]string{"schema_version", "id", "identifier", "status", "result", "text", "error"},
		map[string]any{
			"schema_version": stringProp(),
			"id":             stringProp(),
			"identifier":     stringProp(),
			"status": map[string]any{
				"type": "string",
				"enum": []string{"completed", "failed"},
			},
			"result": nullableObject(),
			"text":   stringProp(),
			"error":  nullableObject(),
		},
	)
	return Template{
		ID:            "app",
		Kind:          profile.KindApp,
		Label:         "App messaging (mcp.message)",
		Instructions:  appInstructions,
		Bounds:        profile.Bounds{ProtocolMin: protocolVersion, ProtocolMax: protocolVersion},
		Scopes:        []string{"mcp.message"},
		RequiresTasks: true,
		Operations: []catalog.Descriptor{{
			Name:         "message",
			Title:        "Message",
			Description:  "Submits a message to one configured Tama reasoning recipient.",
			InputSchema:  mustRaw(input),
			ClientSchema: mustRaw(client),
			OutputSchema: output,
			Annotations:  messageAnnotations,
			TaskSupport:  catalog.TaskSupportRequired,
			Bindings: []catalog.Binding{
				{Source: catalog.SourceClientRequestID, Target: "/identifier", Required: true},
				{Source: catalog.SourceClientContextThreadID, Target: "/thread/identifier", Required: true},
			},
			Strategy: catalog.StrategyUpstreamTask,
		}},
	}
}
