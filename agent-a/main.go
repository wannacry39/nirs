package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	openai "github.com/sashabaranov/go-openai"
)

const defaultMistralBaseURL = "https://api.mistral.ai/v1"
const defaultMistralModel = "mistral-small-latest"

const systemPromptA = `Ты — агент-генератор для игры "пятнашки" (15-puzzle).
В каждом раунде ты обязан выполнить ровно три шага в указанном порядке:

1. Самостоятельно придумываешь поле 4x4 — строку из 16 токенов через пробел: 15 чисел (1..15) и один символ "_".
   Иногда генерируй РЕШЁННОЕ поле "1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 _",
   иногда — НЕРЕШЁННОЕ (числа в произвольном порядке). Выбор делай сам.

2. ОБЯЗАТЕЛЬНО вызови инструмент check_board ровно один раз с этим полем.
   Никаких исключений: даже если поле кажется тебе очевидно решённым или очевидно
   нерешённым — всё равно вызови check_board. Без вызова инструмента раунд не считается
   завершённым. Не пиши финальное резюме до того, как получишь ответ от check_board.

3. После получения результата от check_board — напиши короткое резюме раунда:
   - какое поле ты сгенерировал;
   - что ответил check_board (solved и reason);
   - твоё мнение о вердикте.

Финальное резюме отдавай обычным текстом без JSON и без markdown.`

// firstNonEmpty returns the first non-empty value among args.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func main() {
	apiKey := firstNonEmpty(os.Getenv("MISTRAL_API_KEY"), os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		log.Fatal("MISTRAL_API_KEY is required")
	}

	model := firstNonEmpty(os.Getenv("MISTRAL_MODEL"), os.Getenv("OPENAI_MODEL"), defaultMistralModel)

	baseURL := firstNonEmpty(
		os.Getenv("MISTRAL_BASE_URL"),
		os.Getenv("OPENAI_BASE_URL"),
		defaultMistralBaseURL,
	)
	baseURL = strings.TrimRight(baseURL, "/")

	serverCmd := os.Getenv("AGENT_B_CMD")
	if serverCmd == "" {
		log.Fatal("AGENT_B_CMD is required (path to compiled agent-b binary)")
	}

	rounds := 3
	ctx := context.Background()

	log.Printf("using Mistral endpoint: %s (model=%s)", baseURL, model)

	childEnv := []string{
		"MISTRAL_API_KEY=" + apiKey,
		"MISTRAL_MODEL=" + model,
		"MISTRAL_BASE_URL=" + baseURL,
	}

	mcpClient, err := client.NewStdioMCPClient(
		serverCmd,
		childEnv,
	)
	if err != nil {
		log.Fatalf("mcp client: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	if stderr, ok := client.GetStderr(mcpClient); ok {
		go func() { _, _ = io.Copy(os.Stderr, stderr) }()
	}

	initCtx, cancelInit := context.WithTimeout(ctx, 30*time.Second)
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "agent-a",
		Version: "1.0.0",
	}
	initRes, err := mcpClient.Initialize(initCtx, initReq)
	cancelInit()
	if err != nil {
		log.Fatalf("mcp initialize: %v", err)
	}
	log.Printf("Connected to MCP server %q v%s", initRes.ServerInfo.Name, initRes.ServerInfo.Version)

	listCtx, cancelList := context.WithTimeout(ctx, 30*time.Second)
	toolsRes, err := mcpClient.ListTools(listCtx, mcp.ListToolsRequest{})
	cancelList()
	if err != nil {
		log.Fatalf("list tools: %v", err)
	}

	oaiTools := make([]openai.Tool, 0, len(toolsRes.Tools))
	for _, t := range toolsRes.Tools {
		schemaBytes, err := t.InputSchema.MarshalJSON()
		if err != nil {
			log.Fatalf("marshal schema for %s: %v", t.Name, err)
		}
		var params map[string]any
		if err := json.Unmarshal(schemaBytes, &params); err != nil {
			log.Fatalf("unmarshal schema for %s: %v", t.Name, err)
		}
		oaiTools = append(oaiTools, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
		log.Printf("Discovered MCP tool: %s — %s", t.Name, t.Description)
	}

	mClient := newMistralClient(apiKey, baseURL)

	for round := 1; round <= rounds; round++ {
		fmt.Printf("\n=== Раунд %d ===\n", round)

		messages := []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPromptA},
			{Role: openai.ChatMessageRoleUser, Content: "Старт раунда. Действуй."},
		}

		const maxSteps = 8
		checked := false
		remindersLeft := 2
		for step := 0; step < maxSteps; step++ {
			callCtx, cancelCall := context.WithTimeout(ctx, 90*time.Second)
			resp, err := mClient.CreateChatCompletion(callCtx, openai.ChatCompletionRequest{
				Model:       model,
				Temperature: 0.8,
				Messages:    messages,
				Tools:       oaiTools,
			})
			cancelCall()
			if err != nil {
				log.Printf("[round %d step %d] llm error: %v", round, step, err)
				break
			}
			if len(resp.Choices) == 0 {
				log.Printf("[round %d step %d] no choices", round, step)
				break
			}

			msg := resp.Choices[0].Message
			// Mistral строго требует, чтобы у каждого tool_call поле type было "function".
			// Клиент go-openai иногда оставляет его пустым — выставим явно.
			for i := range msg.ToolCalls {
				if msg.ToolCalls[i].Type == "" {
					msg.ToolCalls[i].Type = openai.ToolTypeFunction
				}
			}
			messages = append(messages, msg)

			if len(msg.ToolCalls) == 0 {
				if !checked && remindersLeft > 0 {
					remindersLeft--
					if strings.TrimSpace(msg.Content) != "" {
						fmt.Printf("Agent A (черновик, без проверки): %s\n", msg.Content)
					}
					reminder := "Ты ещё не вызвал check_board в этом раунде. " +
						"Согласно правилам, проверка обязательна. Сейчас же вызови инструмент " +
						"check_board ровно один раз с тем полем, которое ты сгенерировал, " +
						"и только после получения ответа пиши финальное резюме."
					messages = append(messages, openai.ChatCompletionMessage{
						Role:    openai.ChatMessageRoleUser,
						Content: reminder,
					})
					continue
				}
				if strings.TrimSpace(msg.Content) != "" {
					fmt.Printf("Agent A: %s\n", msg.Content)
				}
				if !checked {
					log.Printf("[round %d] check_board так и не был вызван", round)
				}
				break
			}

			for _, tc := range msg.ToolCalls {
				fmt.Printf("Agent A → MCP tool %s args=%s\n", tc.Function.Name, tc.Function.Arguments)

				var args map[string]any
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
					log.Printf("bad args from llm: %v", err)
					args = map[string]any{}
				}

				toolText := callMCPTool(ctx, mcpClient, tc.Function.Name, args)
				fmt.Printf("Agent B (tool result): %s\n", toolText)

				if tc.Function.Name == "check_board" {
					checked = true
				}

				messages = append(messages, openai.ChatCompletionMessage{
					Role:       openai.ChatMessageRoleTool,
					Content:    toolText,
					ToolCallID: tc.ID,
				})
			}
		}
	}
}

func newMistralClient(apiKey, baseURL string) *openai.Client {
	cfg := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}
	return openai.NewClientWithConfig(cfg)
}

func callMCPTool(ctx context.Context, c *client.Client, name string, args map[string]any) string {
	callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args

	res, err := c.CallTool(callCtx, req)
	if err != nil {
		return fmt.Sprintf(`{"error": %q}`, err.Error())
	}

	var sb strings.Builder
	for _, c := range res.Content {
		if t, ok := mcp.AsTextContent(c); ok {
			sb.WriteString(t.Text)
		}
	}
	body := sb.String()
	if body == "" {
		body = "{}"
	}
	if res.IsError {
		return fmt.Sprintf(`{"error": %q}`, body)
	}
	return body
}
