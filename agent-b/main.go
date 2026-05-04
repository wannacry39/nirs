package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	openai "github.com/sashabaranov/go-openai"
)

type verdict struct {
	Solved bool   `json:"solved"`
	Reason string `json:"reason"`
}

const (
	defaultMistralBaseURL = "https://api.mistral.ai/v1"
	defaultMistralModel   = "mistral-small-latest"
	defaultListenAddr     = ":8080"
	defaultEndpointPath   = "/mcp"
)

// Никаких системных промптов:
//   - чат-REPL agent-b — это «голый» pass-through к LLM;
//   - MCP-инструмент check_board кладёт всю свою инструкцию в user-message
//     при обращении к модели (см. handler ниже).

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
	// Чат пишем в stdout, всё технологическое — в stderr.
	log.SetOutput(os.Stderr)
	log.SetPrefix("[agent-b] ")
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	apiKey := firstNonEmpty(os.Getenv("MISTRAL_API_KEY"), os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		log.Fatal("MISTRAL_API_KEY is required")
	}

	model := firstNonEmpty(os.Getenv("MISTRAL_MODEL"), os.Getenv("OPENAI_MODEL"), defaultMistralModel)
	listenAddr := firstNonEmpty(os.Getenv("AGENT_B_LISTEN_ADDR"), defaultListenAddr)
	endpointPath := firstNonEmpty(os.Getenv("AGENT_B_ENDPOINT_PATH"), defaultEndpointPath)
	chatEnabled := os.Getenv("AGENT_B_CHAT") == "1"

	mClient := newMistralClient(apiKey)

	mcpSrv := buildMCPServer(mClient, model)

	httpSrv := server.NewStreamableHTTPServer(mcpSrv,
		server.WithEndpointPath(endpointPath),
	)

	mux := http.NewServeMux()
	mux.Handle(endpointPath, httpSrv)
	// Лёгкий health-check для оркестратора (Docker / k8s).
	// Это вспомогательный технический эндпоинт, не часть MCP-API.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// HTTP-сервер всегда работает в фоне.
	httpDone := make(chan error, 1)
	go func() {
		log.Printf("Agent B (MCP server) listening on %s%s. provider=mistral model=%s",
			listenAddr, endpointPath, model)
		err := srv.ListenAndServe()
		httpDone <- err
	}()

	// Корректное выключение сервера по сигналу.
	go func() {
		<-ctx.Done()
		log.Printf("shutdown signal received, stopping HTTP server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("http shutdown error: %v", err)
		}
	}()

	if chatEnabled {
		// REPL-чат на основной горутине, HTTP-сервер крутится в фоне.
		runChat(ctx, mClient, model)
		stop()                       // инициируем shutdown HTTP-сервера
		<-waitDone(httpDone)         // дожидаемся остановки сервера
	} else {
		// Обычный «серверный» режим: блокируемся на HTTP до сигнала/ошибки.
		err := <-httpDone
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http listen: %v", err)
		}
	}

	log.Printf("Agent B stopped")
}

func waitDone(ch <-chan error) <-chan error {
	out := make(chan error, 1)
	go func() {
		err := <-ch
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http listen exited with error: %v", err)
		}
		out <- err
	}()
	return out
}

func buildMCPServer(mClient *openai.Client, model string) *server.MCPServer {
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

		// Без system prompt: вся инструкция в user-сообщении.
		// response_format=json_object гарантирует, что модель отдаст
		// валидный JSON-объект.
		userMsg := fmt.Sprintf(
			`Дано поле игры «пятнашки» (15-puzzle) — строка из 16 токенов через пробел:
15 чисел (1..15) и один символ "_". Поле читается слева направо, сверху вниз, по 4 токена в строке.
Решённое поле: "1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 _".

Поле для проверки: %q

Определи, решено ли оно, и верни ответ строго JSON-объектом вида
{"solved": true|false, "reason": "короткая причина на русском"} — без markdown и без текста вне JSON.`,
			board,
		)

		resp, err := mClient.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
			Model:       model,
			Temperature: 0,
			Messages: []openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleUser, Content: userMsg},
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

	return s
}

// runChat — «голый» REPL-чат с пользователем: пользовательское сообщение
// без системного промпта уходит в LLM, ответ возвращается как есть.
func runChat(ctx context.Context, mClient *openai.Client, model string) {
	var messages []openai.ChatCompletionMessage

	fmt.Println("agent-b: чат готов. Просто пиши, что хочешь.")
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
			fmt.Println("agent-b: история очищена.")
			continue
		}

		messages = append(messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: line,
		})

		callCtx, cancelCall := context.WithTimeout(ctx, 90*time.Second)
		resp, err := mClient.CreateChatCompletion(callCtx, openai.ChatCompletionRequest{
			Model:       model,
			Temperature: 0.5,
			Messages:    messages,
		})
		cancelCall()
		if err != nil {
			log.Printf("llm error: %v", err)
			fmt.Printf("agent-b> [ошибка LLM: %v]\n", err)
			// откатываем последнее сообщение, чтобы пользователь мог попробовать снова
			messages = messages[:len(messages)-1]
			continue
		}
		if len(resp.Choices) == 0 {
			fmt.Println("agent-b> [пустой ответ модели]")
			messages = messages[:len(messages)-1]
			continue
		}

		msg := resp.Choices[0].Message
		messages = append(messages, msg)

		content := strings.TrimSpace(msg.Content)
		if content == "" {
			content = "(пустой ответ)"
		}
		fmt.Printf("agent-b> %s\n", content)
	}
}
