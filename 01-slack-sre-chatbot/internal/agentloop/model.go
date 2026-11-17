package agentloop

import (
	"context"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/model/openaimodel"
	"google.golang.org/genai"
)

// NewVertexModel returns a Gemini model served from Vertex AI, authenticated with the
// workload's application default credentials.
func NewVertexModel(ctx context.Context, project, location, modelName string) (model.LLM, error) {
	return gemini.NewModel(ctx, modelName, &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  project,
		Location: location,
	})
}

// NewOllamaModel returns a model served by Ollama's OpenAI-compatible Chat Completions
// endpoint, for example http://localhost:11434/v1. Ollama ignores the API key, but the
// client sends one. Debug runs on it need Config.AnswerTool: ADK sends an output schema
// to a non-Gemini model in strict mode, which would make every field required.
func NewOllamaModel(ctx context.Context, baseURL, modelName string) (model.LLM, error) {
	return openaimodel.NewModel(ctx, modelName, &openaimodel.ClientConfig{
		BaseURL: baseURL,
		APIKey:  "ollama",
		API:     openaimodel.APIChatCompletions,
	})
}
