package llm

// Tool represents an external tool callable by the model.
type Tool struct {
	Type     string
	Function FunctionDefinition
}

// FunctionDefinition defines a callable function.
type FunctionDefinition struct {
	Name        string
	Description string
	Parameters  *ResponseSchema
	Strict      bool
}

// ToolChoice steers how the model chooses tools.
type ToolChoice struct {
	Mode                 string // "auto", "none", "required", "specific"
	SpecificFunctionName string
}

// ToolCall represents a specific tool call returned by the model.
type ToolCall struct {
	ID       string
	Type     string
	Function FunctionCall
}

// FunctionCall represents the function name and arguments requested.
type FunctionCall struct {
	Name      string
	Arguments string
}

// ToolCallChunk represents a streamed chunk of a tool call.
type ToolCallChunk struct {
	Index          int32
	ID             string
	Type           string
	Name           string
	ArgumentsDelta string
}
