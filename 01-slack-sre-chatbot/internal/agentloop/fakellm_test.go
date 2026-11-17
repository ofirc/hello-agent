package agentloop

import (
	"context"
	"fmt"
	"iter"
	"sync"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// scriptedLLM replays one model turn per call. Its name starts with "gemini-" and it does
// not report a Vertex backend, so ADK treats it like the Gemini API: with an output schema
// and tools it injects the set_model_response tool, and the script answers through it.
// Wrap it in vertexLLM to get the native structured-output path production uses.
type scriptedLLM struct {
	turns []*genai.Content
	calls int
	// raw, when set for a turn, is yielded instead of turns[n], for responses that are
	// not plain content, such as a model-side stop with an ErrorCode.
	raw map[int]*model.LLMResponse

	mu   sync.Mutex
	reqs []*model.LLMRequest // every request seen, for asserting on the prompt
}

func (m *scriptedLLM) Name() string { return "gemini-2.5-flash" }

func (m *scriptedLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		m.reqs = append(m.reqs, req)
		n := m.calls
		m.calls++
		resp := m.raw[n]
		m.mu.Unlock()
		if resp != nil {
			yield(resp, nil)
			return
		}
		if n >= len(m.turns) {
			yield(nil, fmt.Errorf("scriptedLLM: no turn %d scripted", n+1))
			return
		}
		yield(&model.LLMResponse{Content: m.turns[n], TurnComplete: true}, nil)
	}
}

// vertexLLM reports the Vertex AI backend, as gemini.NewModel does for NewVertexModel.
// ADK then sends the output schema natively and the final answer arrives as JSON text.
type vertexLLM struct{ *scriptedLLM }

func (vertexLLM) GetGoogleLLMVariant() genai.Backend { return genai.BackendVertexAI }

// call is a model turn that calls one tool.
func call(name string, args map[string]any) *genai.Content {
	return genai.NewContentFromFunctionCall(name, args, genai.RoleModel)
}

// text is a model turn that only says something.
func text(s string) *genai.Content { return genai.NewContentFromText(s, genai.RoleModel) }
