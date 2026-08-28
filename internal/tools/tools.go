// Package tools mirrors diagflow/tools/v3tools.py — the five v3 tool
// definitions wired as Anthropic SDK tools with async handlers.
package tools

import (
	"context"
	"encoding/json"
)

// ToolResult is a uniform tool return value (map of string → result).
type ToolResult struct {
	Data    string
	Success bool
	Error   string
}

// OK returns a success result.
func OK(data string) ToolResult { return ToolResult{Data: data, Success: true} }

// Err returns an error result.
func Err(msg string) ToolResult { return ToolResult{Error: msg, Success: false} }

// Handler is the async tool handler signature.
type Handler func(ctx context.Context, args map[string]any) (ToolResult, error)

// Def is an Anthropic-compatible tool definition with a handler.
type Def struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     Handler
}

// Schema returns the tool schema in Anthropic SDK format.
func (d Def) Schema() map[string]any {
	return map[string]any{
		"name":         d.Name,
		"description":  d.Description,
		"input_schema": d.InputSchema,
	}
}

// object builds a JSON-schema object.
func object(props map[string]any, required []string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// Object builds a JSON-schema object (exported helper).
func Object(props map[string]any, required []string) map[string]any {
	return object(props, required)
}

// prop builds a string-typed property.
func prop(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// Prop builds a string-typed property (exported helper).
func Prop(description string) map[string]any { return prop(description) }

// intProp builds an integer-typed property (with optional default).
func intProp(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

// IntProp builds an integer-typed property (exported helper).
func IntProp(description string) map[string]any { return intProp(description) }

// PropObject builds an object-typed property (for nested params).
func PropObject(description string) map[string]any {
	return map[string]any{"type": "object", "description": description}
}

// SchemaForObject builds a JSON-schema object shape for the SDK's
// ToolInputSchemaParam (used by diagagent for report_diagnosis / branch_decision).
func SchemaForObject(props map[string]any, required []string) map[string]any {
	return object(props, required)
}

// boolProp builds a boolean-typed property.
func boolProp(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

// ArgStr reads a string arg with a default.
func ArgStr(args map[string]any, key, def string) string {
	if args == nil {
		return def
	}
	if s, ok := args[key].(string); ok {
		return s
	}
	return def
}

// jsonString marshals a value to a JSON string, never failing.
func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
