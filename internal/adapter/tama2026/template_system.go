package tama2026

import (
	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/profile"
)

const systemInstructions = "Tama MCP server for bounded inspection of thread-backed execution history and Reflection review workflow. Inspection tools never modify execution history. reflection.comments.review only moves an authorized Reflection comment from open to pending review; it does not alter Steps, Concepts, ContextSnapshots, Flows, or thread history."

func systemTemplate() Template {
	return Template{
		ID:           "system-read",
		Kind:         profile.KindSystem,
		Label:        "System inspection (mcp.server.read, mcp.thread.read, mcp.reflection.read)",
		Instructions: systemInstructions,
		Bounds:       profile.Bounds{ProtocolMin: protocolVersion, ProtocolMax: protocolVersion},
		Scopes:       []string{"mcp.server.read", "mcp.thread.read", "mcp.reflection.read"},
		Operations: []catalog.Descriptor{
			inspection("server.about", "About",
				"Returns non-sensitive Tama server identity and protocol metadata without inspecting graph, runtime, configuration, content, or tenant data.",
				mustRaw(objectSchema(false, nil, map[string]any{})),
				aboutOutput(),
				catalog.StrategyLocalReplayable,
				readOnlyAnnotations,
			),
			inspection("thread.get", "Get Thread",
				"Returns a bounded page of messages and flow pointers for one Tama thread.",
				openInput([]string{"thread_id"}, map[string]any{
					"thread_id": stringField("Thread UUID", 0, 0),
					"after":     stringField("Message UUID cursor from a previous response", 0, 0),
					"limit":     integerField("Page size from 1 to 100"),
				}),
				variantOutput([]string{"schema_version", "thread", "messages", "page"}, map[string]any{
					"schema_version": stringProp(),
					"thread":         objectProp(),
					"messages":       objectArrayProp(),
					"page":           objectProp(),
				}),
				catalog.StrategyLocalReplayable,
				readOnlyAnnotations,
			),
			inspection("thread.steps.list", "List Thread Steps",
				"Lists bounded step pointers in a thread, optionally filtered by exact thought relation.",
				openInput([]string{"thread_id"}, map[string]any{
					"thread_id":         stringField("Thread UUID", 0, 0),
					"relation":          stringField("Exact thought relation such as search-tooling", 0, 0),
					"message_entity_id": stringField("Optional message entity UUID", 0, 0),
					"flow_id":           stringField("Optional flow UUID", 0, 0),
					"after":             stringField("Step UUID cursor from a previous response", 0, 0),
					"limit":             integerField("Page size from 1 to 100"),
				}),
				variantOutput([]string{"schema_version", "thread_id", "steps", "page"}, map[string]any{
					"schema_version": stringProp(),
					"thread_id":      stringProp(),
					"steps":          objectArrayProp(),
					"page":           objectProp(),
				}),
				catalog.StrategyLocalReplayable,
				readOnlyAnnotations,
			),
			inspection("flow.get", "Get Flow",
				"Returns a thread-backed flow and a bounded page of its step pointers.",
				openInput([]string{"flow_id"}, map[string]any{
					"flow_id": stringField("Flow UUID", 0, 0),
					"after":   stringField("Step UUID cursor from a previous response", 0, 0),
					"limit":   integerField("Page size from 1 to 100"),
				}),
				variantOutput([]string{"schema_version", "flow", "thread", "origin_message", "branches_in_page", "steps", "page"}, map[string]any{
					"schema_version":   stringProp(),
					"flow":             objectProp(),
					"thread":           objectProp(),
					"origin_message":   objectProp(),
					"branches_in_page": objectArrayProp(),
					"steps":            objectArrayProp(),
					"page":             objectProp(),
				}),
				catalog.StrategyLocalReplayable,
				readOnlyAnnotations,
			),
			inspection("step.get", "Get Step",
				"Returns persisted diagnostic content for one exact thread-backed step.",
				openInput([]string{"step_id"}, map[string]any{
					"step_id":       stringField("Step UUID", 0, 0),
					"concept_after": stringField("Concept UUID cursor from a previous response", 0, 0),
					"context_after": stringField("Context UUID cursor from a previous response", 0, 0),
					"event_after":   stringField("Step event UUID cursor from a previous response", 0, 0),
					"limit":         integerField("Per-collection page size from 1 to 100"),
				}),
				variantOutput([]string{
					"schema_version", "step", "ancestry", "generation", "toolset",
					"concepts", "contexts", "events", "concepts_page", "contexts_page", "events_page",
				}, map[string]any{
					"schema_version": stringProp(),
					"step":           objectProp(),
					"ancestry":       objectProp(),
					"generation":     objectProp(),
					"toolset":        objectProp(),
					"concepts":       objectArrayProp(),
					"contexts":       objectArrayProp(),
					"events":         objectArrayProp(),
					"concepts_page":  objectProp(),
					"contexts_page":  objectProp(),
					"events_page":    objectProp(),
				}),
				catalog.StrategyLocalReplayable,
				readOnlyAnnotations,
			),
			inspection("artifact.get", "Get Artifact",
				"Returns a bounded UTF-8 chunk of a large artifact referenced by another inspection tool.",
				openInput([]string{"ref"}, map[string]any{
					"ref":       stringField("Opaque artifact reference", 0, 0),
					"after":     stringField("Opaque chunk cursor from a previous response", 0, 0),
					"max_bytes": integerField("Chunk size from 1 to 131072 bytes"),
				}),
				variantOutput([]string{"schema_version", "artifact", "page"}, map[string]any{
					"schema_version": stringProp(),
					"artifact":       objectProp(),
					"page":           objectProp(),
				}),
				catalog.StrategyLocalReplayable,
				readOnlyAnnotations,
			),
			// reflection.comments.review is absent. It requires
			// mcp.reflection.review, which this least-privileged bundle does
			// not request, and Tama hides that tool from tools/list. An
			// advertised review tool is ignored rather than enabled.
			inspection("reflection.comments.list", "List Reflection Comments",
				"Lists bounded Reflection comments with complete thread-backed trace pointers.",
				openInput(nil, map[string]any{
					"state":          stringField("Comment state: open, pending_review, or resolved", 0, 0),
					"thread_id":      stringField("Optional thread UUID filter", 0, 0),
					"step_id":        stringField("Optional Step UUID filter", 0, 0),
					"reference_type": stringField("Optional reference filter: step, context, output, or combined", 0, 0),
					"after":          stringField("Comment UUID cursor from a previous response", 0, 0),
					"limit":          integerField("Page size from 1 to 100"),
				}),
				variantOutput([]string{"schema_version", "comments", "page"}, map[string]any{
					"schema_version": stringProp(),
					"comments":       objectArrayProp(),
					"page":           objectProp(),
				}),
				catalog.StrategyLocalReplayable,
				readOnlyAnnotations,
			),
		},
	}
}

func inspection(name, title, description string, input, output jsonRaw, strategy catalog.Strategy, annotations map[string]any) catalog.Descriptor {
	return catalog.Descriptor{
		Name:         name,
		Title:        title,
		Description:  description,
		InputSchema:  input,
		OutputSchema: output,
		Annotations:  annotations,
		TaskSupport:  catalog.TaskSupportForbidden,
		Strategy:     strategy,
	}
}

type jsonRaw = []byte

func openInput(required []string, properties map[string]any) jsonRaw {
	return mustRaw(objectSchema(true, required, properties))
}

func aboutOutput() jsonRaw {
	return variantOutput(
		[]string{
			"schema_version", "server_name", "app_revision", "supported_mcp_versions",
			"read_only", "execution_history_read_only", "reflection_review_enabled",
		},
		map[string]any{
			"schema_version":              stringProp(),
			"server_name":                 stringProp(),
			"app_revision":                stringProp(),
			"supported_mcp_versions":      stringArrayProp(),
			"read_only":                   boolProp(),
			"execution_history_read_only": boolProp(),
			"reflection_review_enabled":   boolProp(),
		},
	)
}
