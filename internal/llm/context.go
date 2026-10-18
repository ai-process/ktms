package llm

import (
	"bytes"
	"context"
	"encoding/gob"
	"slices"
	"strings"
	"unicode/utf8"
)

type MessageType string

const (
	MessageTypeSystem MessageType = "system"
	MessageTypeUser   MessageType = "user"
	MessageTypeBot    MessageType = "bot"
	MessageTypeTool   MessageType = "tool"
)

const (
	ChatContextVersionLegacy = 0
	ChatContextVersion2      = 2
)

type shadowContextKey struct{}

// WithShadowTraffic marks context as shadow/evaluation traffic.
func WithShadowTraffic(ctx context.Context) context.Context {
	return context.WithValue(ctx, shadowContextKey{}, true)
}

// IsShadowTraffic checks if context is marked as shadow traffic.
func IsShadowTraffic(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, ok := ctx.Value(shadowContextKey{}).(bool)
	return ok && v
}

type ContentPartType string

const (
	ContentPartTypeText     ContentPartType = "text"
	ContentPartTypeImage    ContentPartType = "image"
	ContentPartTypeAudio    ContentPartType = "audio"
	ContentPartTypeDocument ContentPartType = "document"
)

type ImageDetail string

const (
	ImageDetailAuto ImageDetail = "auto"
	ImageDetailLow  ImageDetail = "low"
	ImageDetailHigh ImageDetail = "high"
)

type ContentPart struct {
	Type     ContentPartType
	Text     string
	MimeType string
	Data     []byte
	URL      string
	Detail   ImageDetail
	Format   string
	FileName string
}

type Message struct {
	Text         string
	Type         MessageType
	ToolCalls    []*ToolCall
	ToolCallID   string
	FinishReason string
	Parts        []*ContentPart
}

type ChatContext struct {
	Version                     int // Version for backward compatibility (0 = legacy, 2 = new format)
	ChatID                      int64
	UserID                      int64
	internalUserID              string            // UUID string for usage tracking
	meta                        map[string]string // attached to usage events (e.g. job/task ids)
	ActionID                    string            // Action ID for usage tracking (e.g., "exercise.generate_question")
	MessagesLimit               int
	messages                    []*Message
	protectedIndices            []int
	responseSchema              *ResponseSchema
	maxOutputTokens             int    // 0 = unset, no ceiling sent to the vendor
	enableGoogleSearch          bool   // per-request, never persisted; see SetEnableGoogleSearch
	BaseSystemInstruction       string // Persistent system instructions (not in messages)
	perRequestSystemInstruction string // Ephemeral per-request instruction (not persisted)
	tools                       []*Tool
	toolChoice                  *ToolChoice
	ctx                         context.Context
}

// Context returns the request context associated with this chat, or context.Background().
func (c *ChatContext) Context() context.Context {
	if c == nil || c.ctx == nil {
		return context.Background()
	}
	return c.ctx
}

// WithContext associates a request context with this chat.
func (c *ChatContext) WithContext(ctx context.Context) *ChatContext {
	if c == nil {
		return nil
	}
	c.ctx = ctx
	return c
}

// Clone creates an isolated copy of the context so async background
// operations like shadow routing can modify metadata without data races.
func (c *ChatContext) Clone() *ChatContext {
	if c == nil {
		return nil
	}
	clone := *c
	if c.meta != nil {
		clone.meta = make(map[string]string, len(c.meta))
		for k, v := range c.meta {
			clone.meta[k] = v
		}
	}
	if c.messages != nil {
		clone.messages = append([]*Message(nil), c.messages...)
	}
	if c.protectedIndices != nil {
		clone.protectedIndices = append([]int(nil), c.protectedIndices...)
	}
	if c.tools != nil {
		clone.tools = append([]*Tool(nil), c.tools...)
	}
	return &clone
}

func NewChatContext() *ChatContext {
	return &ChatContext{
		Version:       ChatContextVersion2,
		messages:      []*Message{},
		MessagesLimit: 25,
	}
}
func (c *ChatContext) addMessage(message *Message, isProtected bool) {
	c.messages = append(c.messages, message)
	if isProtected {
		c.protectedIndices = append(c.protectedIndices, len(c.messages)-1)
	}
	if c.MessagesLimit > 0 {
		c.CutMessages(c.MessagesLimit)
	}
}

// RewriteMessages applies an in-place transform to each message in the context.
func (c *ChatContext) RewriteMessages(fn func(m *Message)) {
	for _, m := range c.messages {
		if m != nil {
			fn(m)
		}
	}
}

func (c *ChatContext) CutMessages(maximumLength int) {
	var newMessages []*Message
	var newProtectedIndices []int
	currentLength := len(c.messages)
	protectedLength := len(c.protectedIndices)
	if currentLength <= maximumLength {
		return
	}
	skipLength := currentLength - maximumLength + protectedLength
	for i, message := range c.messages {
		if i < skipLength && !slices.Contains(c.protectedIndices, i) {
			continue
		}
		newMessages = append(newMessages, message)
		if !slices.Contains(c.protectedIndices, i) {
			newProtectedIndices = append(newProtectedIndices, len(newMessages)-1)
		}
	}
	c.protectedIndices = newProtectedIndices
}

func (c *ChatContext) ClearNotProtectedMessages() {
	var newMessages []*Message
	for i, message := range c.messages {
		if slices.Contains(c.protectedIndices, i) {
			newMessages = append(newMessages, message)
		}
	}
	c.protectedIndices = []int{}
	for i := range newMessages {
		c.protectedIndices = append(c.protectedIndices, i)
	}
	c.messages = newMessages
}

func (c *ChatContext) AddSystemMessage(message string) {
	// In version 2, system messages go to BaseSystemInstruction, not messages array
	if c.Version >= ChatContextVersion2 {
		if c.BaseSystemInstruction != "" {
			c.BaseSystemInstruction += "\n\n---\n\n" + message
		} else {
			c.BaseSystemInstruction = message
		}
	} else {
		// Legacy behavior: add to messages
		systemMessage := Message{
			Text: message,
			Type: MessageTypeSystem,
		}
		c.addMessage(&systemMessage, true)
	}
}

// SetRequestInstruction sets a per-request system instruction that applies only to the next GenerateText call
// This instruction is ephemeral and will be cleared after use. It does not persist to Redis.
func (c *ChatContext) SetRequestInstruction(instruction string) {
	c.perRequestSystemInstruction = instruction
}

// GetRequestInstruction returns the current per-request instruction and clears it
func (c *ChatContext) GetRequestInstruction() string {
	instruction := c.perRequestSystemInstruction
	c.perRequestSystemInstruction = "" // Clear after reading
	return instruction
}

// PeekRequestInstruction returns the current per-request instruction without clearing it.
func (c *ChatContext) PeekRequestInstruction() string {
	return c.perRequestSystemInstruction
}

// GetBaseSystemInstruction returns the persistent base system instruction
func (c *ChatContext) GetBaseSystemInstruction() string {
	return c.BaseSystemInstruction
}

// SetBaseSystemInstruction updates the persistent base system instruction
func (c *ChatContext) SetBaseSystemInstruction(instruction string) {
	c.BaseSystemInstruction = instruction
}

func (c *ChatContext) AddAssistantMessage(message string) {
	assistantMessage := Message{
		Text: message,
		Type: MessageTypeBot,
	}
	c.addMessage(&assistantMessage, false)
}

func (c *ChatContext) AddUserMessage(message string) {
	userMessage := Message{
		Text: message,
		Type: MessageTypeUser,
	}
	c.addMessage(&userMessage, false)
}

func (c *ChatContext) AddUserMessageWithParts(text string, parts []*ContentPart) {
	if text == "" {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == ContentPartTypeText && p.Text != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(p.Text)
			}
		}
		text = sb.String()
	}
	userMessage := Message{
		Text:  text,
		Type:  MessageTypeUser,
		Parts: parts,
	}
	c.addMessage(&userMessage, false)
}

func (c *ChatContext) AddToolMessage(toolCallID, text string) {
	msg := Message{
		Text:       text,
		Type:       MessageTypeTool,
		ToolCallID: toolCallID,
	}
	c.addMessage(&msg, false)
}

func (c *ChatContext) AddAssistantMessageWithToolCalls(message string, toolCalls []*ToolCall) {
	assistantMessage := Message{
		Text:      message,
		Type:      MessageTypeBot,
		ToolCalls: toolCalls,
	}
	c.addMessage(&assistantMessage, false)
}

func (c *ChatContext) SetTools(tools []*Tool) {
	c.tools = tools
}

func (c *ChatContext) GetTools() []*Tool {
	return c.tools
}

func (c *ChatContext) SetToolChoice(tc *ToolChoice) {
	c.toolChoice = tc
}

func (c *ChatContext) GetToolChoice() *ToolChoice {
	return c.toolChoice
}

func (c *ChatContext) GetMessages() []*Message {
	return c.messages
}

func (c *ChatContext) SetResponseSchema(schema *ResponseSchema) {
	c.responseSchema = schema
}

func (c *ChatContext) GetResponseSchema() *ResponseSchema {
	return c.responseSchema
}

// SetMaxOutputTokens caps generated tokens for this chat. Output costs ~8x input
// on the expensive tier, so every generation call should set a ceiling generous
// enough to never clip valid content — it exists to stop runaways, not to shape output.
func (c *ChatContext) SetMaxOutputTokens(max int) {
	c.maxOutputTokens = max
}

// GetMaxOutputTokens returns the output ceiling, or 0 when the caller set none.
func (c *ChatContext) GetMaxOutputTokens() int {
	return c.maxOutputTokens
}

// SetEnableGoogleSearch asks for Google Search grounding on this request.
// Advisory: adapters without a search tool ignore it, so callers wanting fresh
// facts should route to a Google candidate explicitly.
func (c *ChatContext) SetEnableGoogleSearch() {
	c.enableGoogleSearch = true
}

// GoogleSearchEnabled reports whether search grounding was requested.
func (c *ChatContext) GoogleSearchEnabled() bool {
	return c.enableGoogleSearch
}

// GetInternalUserID returns the user ID as a string (UUID) for usage tracking
func (c *ChatContext) GetInternalUserID() string {
	return c.internalUserID
}

// SetInternalUserID sets the user ID string (UUID) for usage tracking
func (c *ChatContext) SetInternalUserID(userID string) {
	c.internalUserID = userID
}

// GetActionID returns the action ID for usage tracking
func (c *ChatContext) GetActionID() string {
	return c.ActionID
}

// SetActionID sets the action ID for usage tracking
func (c *ChatContext) SetActionID(actionID string) {
	c.ActionID = actionID
}

// GetMeta returns the metadata attached to usage events for this chat.
func (c *ChatContext) GetMeta() map[string]string {
	return c.meta
}

// SetMeta attaches metadata to every usage event recorded for this chat
// (e.g. job/task ids). Keep it small — it is stored per event.
func (c *ChatContext) SetMeta(meta map[string]string) {
	c.meta = meta
}

// SetCaller sets usage attribution in one call: the internal user id plus
// the usage-event metadata.
func (c *ChatContext) SetCaller(caller Caller) {
	c.internalUserID = caller.UserID
	c.meta = caller.Meta
}

func (c *ChatContext) ClearMessages() {
	c.messages = []*Message{}
}

// RemoveLastMessages removes the last N non-protected messages from the context
// This is useful for cleanup when an operation fails after adding messages
// Protected messages are preserved and will not be removed
func (c *ChatContext) RemoveLastMessages(count int) {
	if count <= 0 {
		return
	}
	if len(c.messages) == 0 {
		return
	}

	// Build a set of indices to remove (last N non-protected messages)
	indicesToRemove := make(map[int]bool)
	removedCount := 0

	// Iterate backwards from the end, marking non-protected messages for removal
	for i := len(c.messages) - 1; i >= 0 && removedCount < count; i-- {
		if !slices.Contains(c.protectedIndices, i) {
			indicesToRemove[i] = true
			removedCount++
		}
	}

	// If no messages can be removed (all are protected), return early
	if len(indicesToRemove) == 0 {
		return
	}

	// Build new messages array, excluding removed messages
	var newMessages []*Message
	var newProtectedIndices []int

	for i, message := range c.messages {
		if indicesToRemove[i] {
			continue // Skip this message
		}

		// Keep this message
		newMessages = append(newMessages, message)
		// Adjust protected index if this message was protected
		if slices.Contains(c.protectedIndices, i) {
			newProtectedIndices = append(newProtectedIndices, len(newMessages)-1)
		}
	}

	c.messages = newMessages
	c.protectedIndices = newProtectedIndices
}

// Gob serializable struct to make all fields exported
type chatContextGob struct {
	Version               int
	ChatID                int64
	UserID                int64
	MessagesLimit         int
	Messages              []*Message
	ProtectedIndices      []int
	ResponseSchema        *ResponseSchema
	BaseSystemInstruction string
	// Note: perRequestSystemInstruction is NOT persisted
}

// MarshalBinary implements the encoding.BinaryMarshaler interface for ChatContext using Gob and a type alias.
func (c *ChatContext) MarshalBinary() ([]byte, error) {
	// Always write in version 2 format
	gobStruct := chatContextGob{
		Version:               ChatContextVersion2,
		ChatID:                c.ChatID,
		UserID:                c.UserID,
		MessagesLimit:         c.MessagesLimit,
		Messages:              c.messages,
		ProtectedIndices:      c.protectedIndices,
		ResponseSchema:        c.responseSchema,
		BaseSystemInstruction: c.BaseSystemInstruction,
		// perRequestSystemInstruction is NOT persisted
	}

	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	err := enc.Encode(gobStruct)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnmarshalBinary implements the encoding.BinaryUnmarshaler interface for ChatContext using Gob and a type alias.
func (c *ChatContext) UnmarshalBinary(data []byte) error {
	var gobStruct chatContextGob
	buf := bytes.NewBuffer(data)
	dec := gob.NewDecoder(buf)
	err := dec.Decode(&gobStruct)
	if err != nil {
		return err
	}

	c.Version = gobStruct.Version
	c.ChatID = gobStruct.ChatID
	c.UserID = gobStruct.UserID
	c.MessagesLimit = gobStruct.MessagesLimit
	c.messages = gobStruct.Messages
	c.protectedIndices = gobStruct.ProtectedIndices
	c.responseSchema = gobStruct.ResponseSchema
	c.BaseSystemInstruction = gobStruct.BaseSystemInstruction
	c.perRequestSystemInstruction = "" // Always clear on load

	// Upgrade legacy format if needed
	if c.Version < ChatContextVersion2 {
		c.UpgradeFromLegacy()
	}

	return nil
}

// UpgradeFromLegacy upgrades a legacy ChatContext (version 0) to version 2 format
// It extracts system messages from the messages array and moves them to BaseSystemInstruction
func (c *ChatContext) UpgradeFromLegacy() {
	if c.Version >= ChatContextVersion2 {
		return // Already upgraded
	}

	var systemMessages []string
	var newMessages []*Message
	var newProtectedIndices []int

	for i, msg := range c.messages {
		if msg.Type == MessageTypeSystem {
			// Check if this is an ephemeral phase instruction
			isEphemeral := false
			text := strings.TrimSpace(msg.Text)
			if strings.HasPrefix(text, "PHASE:") {
				isEphemeral = true
			} else if strings.Contains(text, "Return ONLY JSON") {
				// Check for JSON schema key references: "r", "q" as quoted keys or followed by parentheses
				// Examples: "r", "q", "keys r", "key r", "r (result...)", "q (question...)"
				hasRKey := strings.Contains(text, `"r"`) ||
					strings.Contains(text, "keys r") ||
					strings.Contains(text, "key r") ||
					strings.Contains(text, "r (")
				hasQKey := strings.Contains(text, `"q"`) ||
					strings.Contains(text, "keys q") ||
					strings.Contains(text, "key q") ||
					strings.Contains(text, "q (")
				if hasRKey || hasQKey {
					isEphemeral = true
				}
			}

			if !isEphemeral {
				// This is a base system instruction, extract it
				systemMessages = append(systemMessages, msg.Text)
			}
			// Ephemeral messages are dropped
			// Protected system messages are not added to newProtectedIndices since they're being removed
		} else {
			// Keep user/bot messages
			newMessages = append(newMessages, msg)
			// Adjust protected index if needed
			if slices.Contains(c.protectedIndices, i) {
				newProtectedIndices = append(newProtectedIndices, len(newMessages)-1)
			}
		}
	}

	// Combine system messages into BaseSystemInstruction
	if len(systemMessages) > 0 {
		c.BaseSystemInstruction = strings.Join(systemMessages, "\n\n---\n\n")
	}

	// Update messages and protected indices
	c.messages = newMessages
	c.protectedIndices = newProtectedIndices
	c.Version = ChatContextVersion2
}

// GobEncode implements the gob.GobEncoder interface for ChatContext
func (c *ChatContext) GobEncode() ([]byte, error) {
	return c.MarshalBinary()
}

// GobDecode implements the gob.GobDecoder interface for ChatContext
func (c *ChatContext) GobDecode(data []byte) error {
	return c.UnmarshalBinary(data)
}

// EstimateInputTokens estimates input tokens based on runes in instructions and messages,
// plus heuristic token allowances for media parts.
func (c *ChatContext) EstimateInputTokens() int {
	if c == nil {
		return 1
	}
	runes := utf8.RuneCountInString(c.BaseSystemInstruction) +
		utf8.RuneCountInString(c.perRequestSystemInstruction)
	extraTokens := 0
	for _, m := range c.GetMessages() {
		runes += utf8.RuneCountInString(m.Text)
		for _, tc := range m.ToolCalls {
			runes += utf8.RuneCountInString(tc.Function.Name) + utf8.RuneCountInString(tc.Function.Arguments)
		}
		for _, p := range m.Parts {
			switch p.Type {
			case ContentPartTypeImage:
				if p.Detail == ImageDetailLow {
					extraTokens += 85
				} else {
					extraTokens += 1000
				}
			case ContentPartTypeAudio:
				extraTokens += 200
			case ContentPartTypeDocument:
				extraTokens += 500
			case ContentPartTypeText:
				if m.Text == "" {
					runes += utf8.RuneCountInString(p.Text)
				}
			}
		}
	}
	return runes/4 + extraTokens + 1
}

// EstimateOutputTokens estimates output tokens from character or rune count.
func EstimateOutputTokens(runes int) int {
	if runes <= 0 {
		return 1
	}
	return runes/4 + 1
}
