package setup

import (
	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// UserText builds a user-role content message from plain text — the common way to
// seed an agent invocation.
func UserText(text string) *genai.Content {
	return genai.NewContentFromText(text, genai.RoleUser)
}

// AssistantText builds a model-role content message from plain text.
func AssistantText(text string) *genai.Content {
	return genai.NewContentFromText(text, genai.RoleModel)
}

// ContentText concatenates the text parts of a content (nil-safe).
func ContentText(c *genai.Content) string {
	return contentText(c)
}

// LastText returns the concatenated text of the final content in a slice, or "".
func LastText(contents []*genai.Content) string {
	if len(contents) == 0 {
		return ""
	}
	return contentText(contents[len(contents)-1])
}

// TextEvent builds a session.Event carrying model-authored text, optionally with a
// state delta. Code agents use this to emit output and write workflow state.
func TextEvent(author, text string, state map[string]any) *session.Event {
	ev := &session.Event{
		LLMResponse: model.LLMResponse{Content: AssistantText(text)},
		Author:      author,
	}
	if len(state) > 0 {
		ev.Actions = session.EventActions{StateDelta: state}
	}
	return ev
}

// JSONConfig requests JSON-formatted model output. On the local Ollama path this switches the
// adapter into JSON mode (so the response is at least syntactically valid JSON); it does not
// enforce a schema. Lives here so callers need not import the provider SDK directly.
func JSONConfig() *genai.GenerateContentConfig {
	return &genai.GenerateContentConfig{ResponseMIMEType: "application/json"}
}

// FinalTextResponse builds a terminal (turn-complete) model text response. Model adapters and
// test doubles use it to emit a final answer without naming genai's finish-reason enum.
func FinalTextResponse(text string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      AssistantText(text),
		TurnComplete: true,
		FinishReason: genai.FinishReasonStop,
	}
}

// StateReader is the read side of session state (satisfied by both session.State
// and session.ReadonlyState).
type StateReader interface {
	Get(string) (any, error)
}

// StateString returns the string value at key, or "" if absent or not a string.
func StateString(s StateReader, key string) string {
	v, err := s.Get(key)
	if err != nil {
		return ""
	}
	if str, ok := v.(string); ok {
		return str
	}
	return ""
}
