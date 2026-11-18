package rules

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RegisterTools adds the the rulebook citation tools to server. These read
// rules.json only — no broker, no network, no state beyond the file
// already on disk.
func RegisterTools(server *mcp.Server, doc map[string]json.RawMessage) {
	s := &toolServer{doc: doc}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "rules_list_topics",
		Description: "List the citable entries in rules.json (call this before rules_cite if you don't already know the topic name).",
	}, s.listTopicsTool)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "rules_cite",
		Description: "Quote one rules.json entry by topic: its rule number, status, and every other field, as the source for a numeric claim.",
	}, s.citeTool)
}

type toolServer struct {
	doc map[string]json.RawMessage
}

type ListTopicsInput struct{}

type ListTopicsOutput struct {
	Topics []string `json:"topics"`
}

func (s *toolServer) listTopicsTool(ctx context.Context, req *mcp.CallToolRequest, in ListTopicsInput) (*mcp.CallToolResult, ListTopicsOutput, error) {
	return nil, ListTopicsOutput{Topics: Topics(s.doc)}, nil
}

type CiteInput struct {
	Topic string `json:"topic" jsonschema:"the rules.json entry to cite, e.g. covered_call_closer, concentration_caps, max_risk; see rules_list_topics for the full list"`
}

type CiteOutput struct {
	Topic  string            `json:"topic"`
	Rule   string            `json:"rule,omitempty"`
	Status string            `json:"status,omitempty"`
	Fields map[string]string `json:"fields"`
}

func (s *toolServer) citeTool(ctx context.Context, req *mcp.CallToolRequest, in CiteInput) (*mcp.CallToolResult, CiteOutput, error) {
	c, err := Cite(s.doc, in.Topic)
	if err != nil {
		return nil, CiteOutput{}, err
	}
	return nil, CiteOutput{Topic: c.Topic, Rule: c.Rule, Status: c.Status, Fields: c.Fields}, nil
}
