package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	openai "github.com/sashabaranov/go-openai"
)

type verdict struct {
	Solved bool   `json:"solved"`
	Reason string `json:"reason"`
}

const defaultMistralBaseURL = "https://api.mistral.ai/v1"
const defaultMistralModel = "mistral-small-latest"

const systemPromptChecker = `Ты — проверяющий агент для игры "пятнашки" (15-puzzle).
На вход тебе приходит строка с 16 токенами через пробел: 15 чисел (1..15) и один символ "_".
Поле читается слева направо, сверху вниз, по 4 токена в строке.

Поле считается РЕШЁННЫМ тогда и только тогда, когда токены идут в порядке:
1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 _

Твоя задача — определить, решено ли поле, и кратко объяснить почему.

СТРОГИЕ ПРАВИЛА ОТВЕТА:
- Отвечай ТОЛЬКО валидным JSON-объектом без markdown, без префиксов и без пояснений вне JSON.
- Формат строго:
  {"solved": true|false, "reason": "..."}
- Поле "solved" — булево.
- Поле "reason" — короткая строка на русском (одно-два предложения).
- Никакого текста до или после JSON.`

func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

// firstNonEmpty returns the first non-empty value among args.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func newMistralClient(apiKey string) *openai.Client {
	cfg := openai.DefaultConfig(apiKey)
	base := firstNonEmpty(
		os.Getenv("MISTRAL_BASE_URL"),
		os.Getenv("OPENAI_BASE_URL"),
		defaultMistralBaseURL,
	)
	cfg.BaseURL = strings.TrimRight(base, "/")
	return openai.NewClientWithConfig(cfg)
}

func main() {
	apiKey := firstNonEmpty(os.Getenv("MISTRAL_API_KEY"), os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		log.Fatal("MISTRAL_API_KEY is required")
	}

	model := firstNonEmpty(os.Getenv("MISTRAL_MODEL"), os.Getenv("OPENAI_MODEL"), defaultMistralModel)

	// MCP stdio uses stdout for JSON-RPC; logs go to stderr.
	log.SetOutput(os.Stderr)
	log.SetPrefix("[agent-b] ")

	mClient := newMistralClient(apiKey)

	s := server.NewMCPServer(
		"puzzle-checker",
		"1.0.0",
		server.WithToolCapabilities(false),
		server.WithRecovery(),
	)

	tool := mcp.NewTool("check_board",
		mcp.WithDescription(
			"Проверяет, решено ли поле игры 'пятнашки' (4x4). "+
				"Внутри сам делает запрос к LLM, которая выносит вердикт. "+
				"Возвращает JSON-строку вида {\"solved\": bool, \"reason\": string}.",
		),
		mcp.WithString("board",
			mcp.Required(),
			mcp.Description(
				`Поле в виде строки из 16 токенов через пробел: 15 чисел (1..15) и один символ "_". `+
					`Пример решённого поля: "1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 _".`,
			),
		),
	)

	s.AddTool(tool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		board, err := request.RequireString("board")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		log.Printf("check_board called: board=%q", board)

		resp, err := mClient.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
			Model:       model,
			Temperature: 0,
			Messages: []openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleSystem, Content: systemPromptChecker},
				{Role: openai.ChatMessageRoleUser, Content: "Поле: " + board},
			},
			ResponseFormat: &openai.ChatCompletionResponseFormat{
				Type: openai.ChatCompletionResponseFormatTypeJSONObject,
			},
		})
		if err != nil {
			log.Printf("mistral error: %v", err)
			return mcp.NewToolResultErrorFromErr("mistral error", err), nil
		}
		if len(resp.Choices) == 0 {
			return mcp.NewToolResultError("mistral: no choices"), nil
		}

		raw := extractJSON(resp.Choices[0].Message.Content)
		var v verdict
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			log.Printf("decode llm json error: %v (raw=%s)", err, raw)
			return mcp.NewToolResultErrorf("decode llm json: %v (raw=%s)", err, raw), nil
		}

		log.Printf("check_board verdict: solved=%v reason=%q", v.Solved, v.Reason)

		out, _ := json.Marshal(v)
		return mcp.NewToolResultText(string(out)), nil
	})

	log.Printf("Agent B (MCP server) ready on stdio. provider=mistral model=%s", model)
	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("serve stdio: %v", err)
	}
}
