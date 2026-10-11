package mcp

import (
	"context"
	"testing"

	"github.com/qdrant/go-client/qdrant"
)

// TestCompatAdapter — T040–T042: the static catalog, the invoke_command proxy
// envelope (result/query_type/raw_query_strings/row_count), the error
// pass-through, and the DSL/response-format descriptions.
func TestCompatAdapter(t *testing.T) {
	ctx := context.Background()
	tl := &tools{
		embedder: &mockEmbedder{}, searcher: &mockSearcher{},
		graph: &fakeGraphReader{}, graphSchemaVer: "3",
	}

	// list_commands: a non-empty catalog with name+description+input_schema.
	_, out, err := tl.listCommands(ctx, nil, compatNoArgsInput{})
	if err != nil {
		t.Fatal(err)
	}
	cat, ok := out.(listCommandsResponse)
	if !ok || len(cat.Commands) != 4 {
		t.Fatalf("catalog = %T %+v, want 4 commands", out, out)
	}
	for _, c := range cat.Commands {
		if c.Name == "" || c.Description == "" || len(c.InputSchema) == 0 {
			t.Errorf("incomplete catalog entry %+v", c)
		}
	}

	// invoke_command query_graph: the proxied traversal result in the envelope.
	point := &qdrant.RetrievedPoint{Id: qdrant.NewID("11111111-1111-5111-8111-111111111111"), Payload: qdrant.NewValueMap(map[string]any{
		"kind": "node", "repo": "acme/demo", "symbol": "F", "file_path": "a.go",
	})}
	tl.graph = &fakeGraphReader{points: []*qdrant.RetrievedPoint{point}}
	_, out, err = tl.invokeCommand(ctx, nil, invokeCommandInput{
		CommandName: "query_graph",
		Parameters: invokeParameters{Query: queryGraphInput{
			Start: queryGraphStart{Entity: "definition", Filters: map[string]any{"repo": "acme/demo"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	env, ok := out.(invokeResponse)
	if !ok {
		t.Fatalf("invoke = %T %+v, want invokeResponse", out, out)
	}
	res, ok := env.Result.(queryGraphResponse)
	if !ok || len(res.Results) != 1 {
		t.Fatalf("result = %T %+v, want 1 traversal point", env.Result, env.Result)
	}
	if env.QueryType != "traversal" || env.RowCount != 1 || len(env.RawQueryStrings) != 1 {
		t.Errorf("envelope = %+v, want traversal/1/1 raw string", env)
	}

	// invoke_command query_graph aggregation: query_type and row_count follow.
	tl.graph = &fakeAggStore{fakeGraphReader: &fakeGraphReader{}, facet: map[string]uint64{"acme/demo": 2}, count: 2}
	_, out, err = tl.invokeCommand(ctx, nil, invokeCommandInput{
		CommandName: "query_graph",
		Parameters: invokeParameters{Query: queryGraphInput{
			Mode: "aggregation", Op: "count", GroupBy: "repo",
			Start: queryGraphStart{Filters: map[string]any{"repo": "acme/demo"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	env, ok = out.(invokeResponse)
	if !ok || env.QueryType != "aggregation" || env.RowCount != 1 {
		t.Fatalf("aggregation envelope = %T %+v", out, out)
	}
	if _, ok := env.Result.(aggregationResponse); !ok {
		t.Errorf("aggregation result = %T, want aggregationResponse", env.Result)
	}

	// A structured error passes through unwrapped: non_selective_query survives.
	_, out, err = tl.invokeCommand(ctx, nil, invokeCommandInput{
		CommandName: "query_graph",
		Parameters: invokeParameters{Query: queryGraphInput{
			Start: queryGraphStart{Entity: "definition", Filters: map[string]any{"branch": "main"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if te, ok := out.(toolError); !ok || te.Code != codeNonSelective {
		t.Fatalf("non-selective invoke = %T %+v, want code %q", out, out, codeNonSelective)
	}

	// get_graph_schema through invoke_command; an unknown command is invalid_input.
	tl.graph = &fakeGraphReader{}
	_, out, err = tl.invokeCommand(ctx, nil, invokeCommandInput{CommandName: "get_graph_schema"})
	if err != nil {
		t.Fatal(err)
	}
	env, ok = out.(invokeResponse)
	if !ok || env.QueryType != "graph_schema" || env.RowCount == 0 {
		t.Fatalf("schema envelope = %T %+v", out, out)
	}
	_, out, err = tl.invokeCommand(ctx, nil, invokeCommandInput{CommandName: "no_such_command"})
	if err != nil {
		t.Fatal(err)
	}
	if te, ok := out.(toolError); !ok || te.Code != codeInvalidInput {
		t.Fatalf("unknown command = %T %+v, want invalid_input", out, out)
	}

	// get_query_dsl: cfg version "3" → "v3"; the four modes and the operators.
	_, out, err = tl.getQueryDSL(ctx, nil, compatNoArgsInput{})
	if err != nil {
		t.Fatal(err)
	}
	dsl, ok := out.(queryDSL)
	if !ok || dsl.Version != "v3" || len(dsl.Modes) != 4 || len(dsl.Operators) == 0 {
		t.Fatalf("dsl = %T %+v, want v3/4 modes/operators", out, out)
	}

	// get_response_format: static v5 envelope schema.
	_, out, err = tl.getResponseFormat(ctx, nil, compatNoArgsInput{})
	if err != nil {
		t.Fatal(err)
	}
	rf, ok := out.(responseFormatView)
	if !ok || rf.Version != "v5" || rf.Schema["type"] != "object" {
		t.Fatalf("response format = %T %+v, want v5 object schema", out, out)
	}
}
