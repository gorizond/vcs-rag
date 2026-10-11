package mcp

// Orbit-compatible adapter (T040–T042, US5, contracts/orbit-compat-mcp.md):
// a thin proxy surface so clients written against GitLab Orbit run unchanged.
// No engine logic lives here — every command forwards to the existing
// query_graph / graph_schema handlers, and the wrapper is form-compatible
// with the Orbit query_response/v5 envelope (INFERRED: the exact Orbit
// wrapper is unpublished).

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gorizond/vcs-rag/internal/graph"
)

// Tool descriptions — also the list_commands catalog source (T040).
const (
	toolDescListCommands = "List the graph commands supported by this server with descriptions and input schemas " +
		"(Orbit-compatible catalog: query_graph, get_graph_schema, get_query_dsl, get_response_format)."
	toolDescInvokeCommand = "Invoke a graph command by name (Orbit-compatible). command_name=query_graph runs the canonical query DSL " +
		"(parameters.query) and wraps the result in the Orbit envelope {result, query_type, raw_query_strings, row_count}; " +
		"command_name=get_graph_schema returns the self-describing schema. Structured errors (e.g. non_selective_query) pass through unchanged."
	toolDescGetQueryDSL = "The JSON query DSL grammar/version (Orbit-compatible): version (synced with the internal graph schema version), " +
		"modes, operators."
	toolDescGetResponseFormat = "The invoke_command response format (Orbit-compatible): envelope version (v5) and its static JSON schema."
)

// compatNoArgsInput — the argument-less compat tools (list_commands,
// get_query_dsl, get_response_format).
type compatNoArgsInput struct{}

// commandSpec — one list_commands catalog entry: the command name, its
// description (mirrors the registered tool) and its input JSON schema.
type commandSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// listCommandsResponse — the static catalog.
type listCommandsResponse struct {
	Commands []commandSpec `json:"commands"`
}

// invokeCommandInput — the Orbit invocation request: the command name plus
// its parameters (parameters.query is the query_graph DSL document).
type invokeCommandInput struct {
	CommandName string           `json:"command_name" jsonschema:"command to invoke: query_graph | get_graph_schema"`
	Parameters  invokeParameters `json:"parameters,omitempty" jsonschema:"command parameters"`
}

// invokeParameters — the Orbit parameters object: query feeds query_graph,
// repo scopes get_graph_schema.
type invokeParameters struct {
	Query queryGraphInput `json:"query,omitempty" jsonschema:"query_graph: the graph query (canonical JSON DSL)"`
	Repo  string          `json:"repo,omitempty" jsonschema:"get_graph_schema: optional repo scope for coverage_per_repo"`
}

// invokeResponse — the Orbit-ish invoke_command envelope: the proxied result,
// the resolved query type, the raw query strings, and the row count.
type invokeResponse struct {
	Result          any      `json:"result"`
	QueryType       string   `json:"query_type"`
	RawQueryStrings []string `json:"raw_query_strings"`
	RowCount        int      `json:"row_count"`
}

// responseFormatView — the get_response_format answer: the envelope version
// and its static JSON schema.
type responseFormatView struct {
	Version string         `json:"version"`
	Schema  map[string]any `json:"schema"`
}

// compatCatalog — the static list_commands catalog (T040); input schemas are
// compact JSON Schema fragments of the command arguments.
var compatCatalog = []commandSpec{
	{
		Name: "query_graph",
		Description: "Unified structured graph query: mode=traversal (default) walks the start entity (definition|file|mr_file|ci|vuln) " +
			"plus filters over an optional edge selector, hops 1..3, limit default 30 max 1000; mode=aggregation groups by op over a field; " +
			"mode=path_finding finds the shortest path between from/to; mode=neighbors lists the 1-hop peers of node_key. " +
			"At least one selective filter predicate is required.",
		InputSchema: objectSchema(map[string]any{
			"mode":      enumSchema([]string{"traversal", "aggregation", "path_finding", "neighbors"}, "query family (default traversal)"),
			"start":     objectSchema(map[string]any{"entity": map[string]any{"type": "string"}, "filters": map[string]any{"type": "object"}}, nil),
			"edge":      objectSchema(map[string]any{"type": map[string]any{"type": "string"}, "direction": map[string]any{"type": "string"}}, nil),
			"hops":      map[string]any{"type": "integer"},
			"limit":     map[string]any{"type": "integer"},
			"filters":   map[string]any{"type": "object", "description": "global filters; at least one selective predicate required"},
			"from":      map[string]any{"type": "string"},
			"to":        map[string]any{"type": "string"},
			"node_key":  map[string]any{"type": "string"},
			"rel_types": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"op":        enumSchema([]string{"count", "sum", "avg", "min", "max"}, "aggregation op (default count)"),
			"field":     map[string]any{"type": "string"},
			"group_by":  map[string]any{"type": "string"},
			"sort":      map[string]any{"type": "string"},
		}, nil),
	},
	{
		Name:        "get_graph_schema",
		Description: "Self-describing graph schema: node types with indexed properties, edge types, entity types, the query DSL version/modes/operators, and coverage_per_repo markers.",
		InputSchema: objectSchema(map[string]any{
			"repo": map[string]any{"type": "string", "description": "optional repo scope for coverage_per_repo"},
		}, nil),
	},
	{
		Name:        "get_query_dsl",
		Description: "The JSON query DSL grammar/version: version, modes, operators.",
		InputSchema: objectSchema(map[string]any{}, nil),
	},
	{
		Name:        "get_response_format",
		Description: "The invoke_command response format: envelope version (v5) and its static JSON schema.",
		InputSchema: objectSchema(map[string]any{}, nil),
	},
}

// objectSchema — a minimal JSON Schema object node.
func objectSchema(props map[string]any, required []string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// enumSchema — a minimal JSON Schema string-enum node.
func enumSchema(values []string, desc string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": desc}
}

// listCommands — the static command catalog (T040).
func (t *tools) listCommands(_ context.Context, _ *mcp.CallToolRequest, _ compatNoArgsInput) (*mcp.CallToolResult, any, error) {
	return nil, listCommandsResponse{Commands: compatCatalog}, nil
}

// invokeCommand — the Orbit invocation proxy (T041): query_graph and
// get_graph_schema run through their existing handlers and come back wrapped
// in the invokeResponse envelope. A structured upstream error (toolError)
// stays unwrapped — the contractual codes (non_selective_query,
// aggregation_cap_exceeded, invalid_input) must reach the client verbatim.
func (t *tools) invokeCommand(ctx context.Context, _ *mcp.CallToolRequest, in invokeCommandInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	switch name := strings.TrimSpace(in.CommandName); name {
	case "query_graph":
		return t.invokeQueryGraph(ctx, in.Parameters.Query)
	case "get_graph_schema", "graph_schema": // the registered tool name stays accepted
		_, out, err := t.graphSchema(ctx, nil, graphSchemaInput{Repo: in.Parameters.Repo})
		if err != nil {
			return nil, errorResult(ctx, codeSearchFailed, err), nil
		}
		if te, ok := out.(toolError); ok {
			return nil, te, nil
		}
		return nil, invokeResponse{
			Result:          out,
			QueryType:       "graph_schema",
			RawQueryStrings: []string{"get_graph_schema"},
			RowCount:        rowCount(out),
		}, nil
	default:
		return nil, toolError{
			Code:    codeInvalidInput,
			Message: fmt.Sprintf("unknown command %q: supported commands are query_graph and get_graph_schema", name),
		}, nil
	}
}

// invokeQueryGraph proxies one query_graph call and wraps its typed result;
// query_type is the resolved mode (the handler validated it on the success
// path) and raw_query_strings carries the query as sent (serialized DSL).
func (t *tools) invokeQueryGraph(ctx context.Context, q queryGraphInput) (*mcp.CallToolResult, any, error) {
	_, out, err := t.queryGraph(ctx, nil, q)
	if err != nil {
		return nil, errorResult(ctx, codeSearchFailed, err), nil
	}
	if te, ok := out.(toolError); ok {
		return nil, te, nil
	}
	mode, _ := parseGraphMode(q.Mode)
	return nil, invokeResponse{
		Result:          out,
		QueryType:       string(mode),
		RawQueryStrings: []string{marshalJSON(q)},
		RowCount:        rowCount(out),
	}, nil
}

// rowCount is the envelope's row_count: the size of the result's primary
// listing, per response family (0 for an unknown shape).
func rowCount(out any) int {
	switch r := out.(type) {
	case queryGraphResponse:
		return len(r.Results)
	case aggregationResponse:
		return len(r.Groups)
	case pathResponse:
		return len(r.Path)
	case neighborsResponse:
		return len(r.Neighbors)
	case graphSchemaResponse:
		return len(r.NodeTypes)
	default:
		return 0
	}
}

// getQueryDSL — the DSL description (T042). The numeric version syncs with
// the internal graph schema version (GRAPH_SCHEMA_VER, default 3); the Orbit
// form carries the "v" prefix (contract: "v3").
func (t *tools) getQueryDSL(_ context.Context, _ *mcp.CallToolRequest, _ compatNoArgsInput) (*mcp.CallToolResult, any, error) {
	ver := t.graphSchemaVer
	if ver == "" {
		ver = "3" // default WORKER_GRAPH_SCHEMA_VER (schema v3, FR-022/FR-026)
	}
	// Same contractual lists graph_schema declares (mirrors tools.go).
	return nil, queryDSL{
		Version: "v" + ver,
		Modes: []string{
			string(graph.ModeTraversal), string(graph.ModeAggregation),
			string(graph.ModePathFinding), string(graph.ModeNeighbors),
		},
		Operators: []string{
			"match", "match_text", "in", "range", "is_null",
			"count", "sum", "avg", "min", "max", "group_by", "sort", "shortest",
		},
	}, nil
}

// getResponseFormat — the invoke_command envelope description (T042):
// version v5, form-compatible with the Orbit query_response wrapper (INFERRED).
func (t *tools) getResponseFormat(_ context.Context, _ *mcp.CallToolRequest, _ compatNoArgsInput) (*mcp.CallToolResult, any, error) {
	return nil, responseFormatView{
		Version: "v5",
		Schema: map[string]any{
			"type":        "object",
			"description": "Orbit-compatible invoke_command response envelope (form-compatible, INFERRED).",
			"properties": map[string]any{
				"result":            map[string]any{"description": "the command result payload (query_graph response or graph schema)"},
				"query_type":        map[string]any{"type": "string", "description": "resolved query family (traversal|aggregation|path_finding|neighbors) or graph_schema"},
				"raw_query_strings": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "the raw query as sent (serialized canonical DSL)"},
				"row_count":         map[string]any{"type": "integer", "description": "number of result rows"},
			},
			"required": []string{"result", "query_type", "raw_query_strings", "row_count"},
		},
	}, nil
}
