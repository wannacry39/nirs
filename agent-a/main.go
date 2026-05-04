package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	openai "github.com/sashabaranov/go-openai"
)

const (
	defaultMistralBaseURL = "https://api.mistral.ai/v1"
	defaultMistralModel   = "mistral-small-latest"
	defaultAgentBURL      = "http://localhost:8080/mcp"
)

// Никаких системных промптов: agent-a — это «голый» чат с моделью.
// Решение, нужно ли вызывать check_board, модель принимает сама на основе
// описания инструмента (поля Name/Description/Parameters), которое пришло
// из MCP-сервера через tools/list. Это штатный механизм tool-calling
// в OpenAI/Mistral API: системный промпт для этого не нужен.

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func main() {
	// Чат идёт в stdout, всё остальное (служебные логи) — в stderr,
	// чтобы не засорять вывод REPL.
	log.SetOutput(os.Stderr)
	log.SetPrefix("[agent-a] ")
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

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

	agentBURL := firstNonEmpty(os.Getenv("AGENT_B_URL"), defaultAgentBURL)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("using Mistral endpoint: %s (model=%s)", baseURL, model)

	// MCP — опционален. Если agent-b недоступен, agent-a продолжает работать
	// как обычный чат с LLM, без инструмента check_board.
	mcpClient, oaiTools := tryConnectMCP(ctx, agentBURL)
	if mcpClient != nil {
		defer func() { _ = mcpClient.Close() }()
		log.Printf("MCP ready: %d tool(s) available", len(oaiTools))
	} else {
		log.Printf("MCP unavailable — running standalone chat without tools")
	}

	mClient := newMistralClient(apiKey, baseURL)

	runChat(ctx, mClient, mcpClient, model, oaiTools)
}

// tryConnectMCP пытается установить MCP-соединение с agent-b.
// При успехе возвращает рабочий клиент и набор tools для chat completion.
// При любой ошибке — (nil, nil): агент будет работать как обычный чат
// без инструментов. Несколько коротких ретраев сглаживают compose-race
// при одновременном старте контейнеров.
func tryConnectMCP(ctx context.Context, agentBURL string) (*client.Client, []openai.Tool) {
	log.Printf("trying MCP server agent-b at: %s", agentBURL)

	mcpClient, err := client.NewStreamableHttpClient(agentBURL)
	if err != nil {
		log.Printf("MCP client init failed: %v", err)
		return nil, nil
	}

	const maxAttempts = 6
	const attemptDelay = 500 * time.Millisecond
	var initRes *mcp.InitializeResult

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			break
		}

		startCtx, cancelStart := context.WithTimeout(ctx, 3*time.Second)
		startErr := mcpClient.Start(startCtx)
		cancelStart()
		if startErr != nil {
			log.Printf("MCP start attempt %d/%d failed: %v", attempt, maxAttempts, startErr)
			time.Sleep(attemptDelay)
			continue
		}

		initCtx, cancelInit := context.WithTimeout(ctx, 5*time.Second)
		initReq := mcp.InitializeRequest{}
		initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
		initReq.Params.ClientInfo = mcp.Implementation{
			Name:    "agent-a",
			Version: "1.0.0",
		}
		res, initErr := mcpClient.Initialize(initCtx, initReq)
		cancelInit()
		if initErr != nil {
			log.Printf("MCP initialize attempt %d/%d failed: %v", attempt, maxAttempts, initErr)
			time.Sleep(attemptDelay)
			continue
		}
		initRes = res
		break
	}

	if initRes == nil {
		_ = mcpClient.Close()
		return nil, nil
	}
	log.Printf("connected to MCP server %q v%s", initRes.ServerInfo.Name, initRes.ServerInfo.Version)

	listCtx, cancelList := context.WithTimeout(ctx, 5*time.Second)
	toolsRes, err := mcpClient.ListTools(listCtx, mcp.ListToolsRequest{})
	cancelList()
	if err != nil {
		log.Printf("MCP list tools failed: %v", err)
		_ = mcpClient.Close()
		return nil, nil
	}

	oaiTools := make([]openai.Tool, 0, len(toolsRes.Tools))
	for _, t := range toolsRes.Tools {
		schemaBytes, err := t.InputSchema.MarshalJSON()
		if err != nil {
			log.Printf("marshal schema for %s: %v (skipped)", t.Name, err)
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(schemaBytes, &params); err != nil {
			log.Printf("unmarshal schema for %s: %v (skipped)", t.Name, err)
			continue
		}
		oaiTools = append(oaiTools, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
		log.Printf("discovered MCP tool: %s — %s", t.Name, t.Description)
	}

	if len(oaiTools) == 0 {
		// Сервер ответил, но инструментов в нём нет — нет смысла тянуть
		// клиент дальше, всё равно работаем как plain chat.
		_ = mcpClient.Close()
		return nil, nil
	}

	return mcpClient, oaiTools
}

func runChat(
	ctx context.Context,
	mClient *openai.Client,
	mcpClient *client.Client,
	model string,
	oaiTools []openai.Tool,
) {
	var messages []openai.ChatCompletionMessage

	fmt.Println("agent-a: чат готов. Просто пиши, что хочешь.")
	if len(oaiTools) > 0 {
		fmt.Println("         (если попросишь проверить поле «пятнашек» — схожу к agent-b по MCP)")
	} else {
		fmt.Println("         (agent-b недоступен — работаю как обычный чат, без инструментов)")
	}
	fmt.Println("         /reset — очистить историю, /exit или Ctrl+D — выйти.")

	reader := bufio.NewReader(os.Stdin)

	for {
		select {
		case <-ctx.Done():
			fmt.Println()
			return
		default:
		}

		fmt.Print("\nyou> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Println()
				return
			}
			log.Printf("stdin read error: %v", err)
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch line {
		case "/exit", "/quit":
			return
		case "/reset":
			messages = nil
			fmt.Println("agent-a: история очищена.")
			continue
		}

		messages = append(messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: line,
		})

		messages = runToolLoop(ctx, mClient, mcpClient, model, oaiTools, messages)
	}
}

// runToolLoop крутит цикл «модель → возможные tool_calls → MCP → модель»
// до тех пор, пока модель не ответит обычным текстом или не упрётся в лимит шагов.
// Если oaiTools пуст или mcpClient nil, цикл фактически вырождается в один
// LLM-запрос без инструментов: модель просто отвечает текстом.
// Возвращает обновлённую историю сообщений.
func runToolLoop(
	ctx context.Context,
	mClient *openai.Client,
	mcpClient *client.Client,
	model string,
	oaiTools []openai.Tool,
	messages []openai.ChatCompletionMessage,
) []openai.ChatCompletionMessage {
	const maxSteps = 6
	for step := 0; step < maxSteps; step++ {
		callCtx, cancelCall := context.WithTimeout(ctx, 90*time.Second)
		req := openai.ChatCompletionRequest{
			Model:       model,
			Temperature: 0.7,
			Messages:    messages,
		}
		if len(oaiTools) > 0 {
			req.Tools = oaiTools
		}
		resp, err := mClient.CreateChatCompletion(callCtx, req)
		cancelCall()
		if err != nil {
			log.Printf("llm error: %v", err)
			fmt.Printf("agent-a> [ошибка LLM: %v]\n", err)
			return messages
		}
		if len(resp.Choices) == 0 {
			log.Printf("llm returned no choices")
			fmt.Println("agent-a> [пустой ответ модели]")
			return messages
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
			content := strings.TrimSpace(msg.Content)
			if content == "" {
				content = "(пустой ответ)"
			}
			fmt.Printf("agent-a> %s\n", content)
			return messages
		}

		for _, tc := range msg.ToolCalls {
			log.Printf("→ MCP tool %s args=%s", tc.Function.Name, tc.Function.Arguments)

			var args map[string]any
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				log.Printf("bad args from llm: %v", err)
				args = map[string]any{}
			}

			toolText := callMCPTool(ctx, mcpClient, tc.Function.Name, args)
			log.Printf("← MCP tool %s result=%s", tc.Function.Name, toolText)

			messages = append(messages, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				Content:    toolText,
				ToolCallID: tc.ID,
			})
		}
	}

	log.Printf("tool loop exceeded maxSteps")
	fmt.Println("agent-a> [превышен лимит шагов tool-calling]")
	return messages
}

func newMistralClient(apiKey, baseURL string) *openai.Client {
	cfg := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}
	return openai.NewClientWithConfig(cfg)
}

func callMCPTool(ctx context.Context, c *client.Client, name string, args map[string]any) string {
	if c == nil {
		// Защита: модель попыталась позвать инструмент, но MCP-клиента нет.
		// На практике сюда не попадаем, потому что в этом случае oaiTools пуст
		// и модель не получает описания инструментов.
		return `{"error": "MCP-клиент недоступен"}`
	}

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
