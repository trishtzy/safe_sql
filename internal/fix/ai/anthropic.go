package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// Anthropic is the Claude-backed provider. Credentials and an optional
// gateway URL come from the environment (ANTHROPIC_API_KEY,
// ANTHROPIC_BASE_URL), never from repository configuration.
type Anthropic struct {
	client anthropic.Client
	model  string
}

// NewAnthropic returns a provider for the given model id.
func NewAnthropic(model string) *Anthropic {
	if model == "" {
		model = "claude-opus-5"
	}
	return &Anthropic{client: anthropic.NewClient(), model: model}
}

const toolName = "propose_migration_files"

func proposeTool() anthropic.ToolUnionParam {
	tool := anthropic.ToolParam{
		Name:        toolName,
		Description: anthropic.String("Propose the complete new content of migration files that fix the findings."),
		Strict:      anthropic.Bool(true),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"files": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":    map[string]any{"type": "string", "description": "Path exactly as given, or a new file in the same directory"},
							"action":  map[string]any{"type": "string", "enum": []string{"modify", "create"}},
							"content": map[string]any{"type": "string", "description": "Complete file content"},
						},
						"required":             []string{"path", "action", "content"},
						"additionalProperties": false,
					},
				},
				"summary": map[string]any{"type": "string", "description": "One paragraph for the pull request describing what changed and why"},
			},
			Required: []string{"files", "summary"},
			ExtraFields: map[string]any{
				"additionalProperties": false,
			},
		},
	}
	return anthropic.ToolUnionParam{OfTool: &tool}
}

// Propose implements Provider.
func (a *Anthropic) Propose(ctx context.Context, req Request) (*Response, error) {
	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 16000,
		System:    []anthropic.TextBlockParam{{Text: req.System}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(req.Prompt))},
		Tools:     []anthropic.ToolUnionParam{proposeTool()},
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}
	out := &Response{Model: string(resp.Model)}
	if resp.StopReason == anthropic.StopReasonRefusal {
		out.Refused = true
		out.Text = resp.StopDetails.Explanation
		return out, nil
	}
	var text []string
	for _, block := range resp.Content {
		switch v := block.AsAny().(type) {
		case anthropic.TextBlock:
			text = append(text, v.Text)
		case anthropic.ToolUseBlock:
			if v.Name != toolName {
				continue
			}
			var p Proposal
			if err := json.Unmarshal([]byte(v.JSON.Input.Raw()), &p); err != nil {
				return nil, fmt.Errorf("anthropic: tool input: %w", err)
			}
			out.Proposal = &p
		}
	}
	out.Text = strings.Join(text, "\n")
	return out, nil
}
