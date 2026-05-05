// Универсальный агент для игры «пятнашки».
//
//	CREATOR — MCP-сервис (:9001/mcp). Инструмент: generate_new_game.
//	          LLM генерирует перемешанную доску и напутствие.
//
//	CHECKER — MCP-сервис (:9002/mcp). Инструменты: is_finished, check_is_valid.
//	          LLM проверяет завершённость и валидность хода.
//
//	PLAYER  — интерактивный REPL.
//	          Подключается к mcp-server по MCP, получает tools/list,
//	          вызывает инструменты — сервер роутит к нужному агенту.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	openai "github.com/sashabaranov/go-openai"
)

const (
	defaultMistralBaseURL = "https://api.mistral.ai/v1"
	defaultMistralModel   = "mistral-small-latest"
)

type Role string

const (
	RolePlayer  Role = "PLAYER"
	RoleCreator Role = "CREATOR"
	RoleChecker Role = "CHECKER"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func newLLMClient(apiKey, baseURL string) *openai.Client {
	cfg := openai.DefaultConfig(apiKey)
	cfg.BaseURL = strings.TrimRight(baseURL, "/")
	return openai.NewClientWithConfig(cfg)
}

// askLLMJSON — запрос к LLM с обязательным JSON-ответом.
func askLLMJSON(ctx context.Context, llm *openai.Client, model, prompt string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	resp, err := llm.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model:       model,
		Temperature: 0,
		Messages:    []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: prompt}},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONObject,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("LLM: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("LLM: no choices")
	}
	return []byte(strings.TrimSpace(resp.Choices[0].Message.Content)), nil
}

// serveMCP запускает MCP HTTP-сервер с дополнительным /healthz.
func serveMCP(ctx context.Context, mcpSrv *server.MCPServer, listenAddr, endpointPath string) {
	httpHandler := server.NewStreamableHTTPServer(mcpSrv,
		server.WithEndpointPath(endpointPath),
	)
	mux := http.NewServeMux()
	mux.Handle(endpointPath, httpHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	done := make(chan error, 1)
	go func() {
		log.Printf("MCP server listening on %s%s", listenAddr, endpointPath)
		done <- srv.ListenAndServe()
	}()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http: %v", err)
	}
	log.Printf("stopped")
}

// ── main ─────────────────────────────────────────────────────────────────────

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	role := Role(strings.ToUpper(strings.TrimSpace(os.Getenv("AGENT_ROLE"))))
	if role == "" {
		log.Fatal("AGENT_ROLE is required: PLAYER | CREATOR | CHECKER")
	}
	log.SetPrefix(fmt.Sprintf("[%s] ", role))

	apiKey := firstNonEmpty(os.Getenv("MISTRAL_API_KEY"), os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		log.Fatal("MISTRAL_API_KEY is required")
	}
	model := firstNonEmpty(os.Getenv("MISTRAL_MODEL"), os.Getenv("OPENAI_MODEL"), defaultMistralModel)
	baseURL := firstNonEmpty(os.Getenv("MISTRAL_BASE_URL"), os.Getenv("OPENAI_BASE_URL"), defaultMistralBaseURL)

	llm := newLLMClient(apiKey, baseURL)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("starting (model=%s)", model)

	switch role {
	case RoleCreator:
		runCreator(ctx, llm, model)
	case RoleChecker:
		runChecker(ctx, llm, model)
	case RolePlayer:
		runPlayer(ctx, llm, model)
	default:
		log.Fatalf("unknown AGENT_ROLE=%q", role)
	}
}

// ── CREATOR ───────────────────────────────────────────────────────────────────
//
// MCP-сервис на LISTEN_ADDR (default :9001), путь ENDPOINT_PATH (default /mcp).
// Инструмент: generate_new_game(theme?) → {board, message}
// LLM генерирует случайно перемешанную доску и напутствие для игрока.

func runCreator(ctx context.Context, llm *openai.Client, model string) {
	listenAddr := firstNonEmpty(os.Getenv("LISTEN_ADDR"), ":9001")
	endpointPath := firstNonEmpty(os.Getenv("ENDPOINT_PATH"), "/mcp")

	s := server.NewMCPServer("agent-creator", "1.0.0",
		server.WithToolCapabilities(false),
		server.WithRecovery(),
	)

	s.AddTool(
		mcp.NewTool("generate_new_game",
			mcp.WithDescription(
				"Создаёт новую игру «пятнашки»: LLM генерирует случайно перемешанную доску "+
					"и напутствие для игрока. "+
					"Возвращает {\"board\": string, \"message\": string}.",
			),
			mcp.WithString("theme",
				mcp.Description("Необязательная тема напутствия (например, «загадочное», «мотивирующее»)."),
			),
		),
		func(innerCtx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			argsMap, _ := req.Params.Arguments.(map[string]any)
			if argsMap == nil {
				argsMap = map[string]any{}
			}
			theme, _ := argsMap["theme"].(string)
			if theme == "" {
				theme = "азартное и бодрящее"
			}

			prompt := fmt.Sprintf(`Ты — агент-создатель игры «пятнашки» (15-puzzle, 4×4).

Сгенерируй случайно перемешанное поле и напутствие для игрока.

Правила поля:
- 16 позиций: числа 1–15 и символ "_" (пустая клетка), каждое ровно по одному разу.
- Поле должно быть хорошо перемешано, далеко от решённого: "1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 _".
- Формат: 16 токенов через пробел, слева направо, сверху вниз.
- Пример: "5 1 2 3 9 6 7 4 13 10 11 8 14 15 12 _"

Ответь строго JSON без markdown:
{
  "board": "<16 токенов через пробел>",
  "message": "<напутствие игроку, стиль: %s, 2-3 предложения>"
}`, theme)

			raw, err := askLLMJSON(innerCtx, llm, model, prompt)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("LLM failed", err), nil
			}
			log.Printf("[CREATOR] %s", raw)
			return mcp.NewToolResultText(string(raw)), nil
		},
	)

	serveMCP(ctx, s, listenAddr, endpointPath)
}

// ── CHECKER ───────────────────────────────────────────────────────────────────
//
// MCP-сервис на LISTEN_ADDR (default :9002), путь ENDPOINT_PATH (default /mcp).
// Инструменты:
//
//	is_finished(board)           → {finished, reason}
//	check_is_valid(board, tile)  → {valid, board, reason}
//
// Вся логика — через LLM.

func runChecker(ctx context.Context, llm *openai.Client, model string) {
	listenAddr := firstNonEmpty(os.Getenv("LISTEN_ADDR"), ":9002")
	endpointPath := firstNonEmpty(os.Getenv("ENDPOINT_PATH"), "/mcp")

	s := server.NewMCPServer("agent-checker", "1.0.0",
		server.WithToolCapabilities(false),
		server.WithRecovery(),
	)

	// is_finished — LLM сравнивает доску с решённым состоянием.
	s.AddTool(
		mcp.NewTool("is_finished",
			mcp.WithDescription(
				"Проверяет, решена ли игра «пятнашки». "+
					"LLM сравнивает переданную доску с эталоном. "+
					"Возвращает {\"finished\": bool, \"reason\": string}.",
			),
			mcp.WithString("board",
				mcp.Required(),
				mcp.Description("Текущая доска: 16 токенов через пробел."),
			),
		),
		func(innerCtx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			board, err := req.RequireString("board")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			prompt := fmt.Sprintf(`Ты — агент-проверяющий игры «пятнашки» (15-puzzle, 4×4).

Решённое поле: "1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 _"
Проверяемое поле: %q

Определи, совпадает ли проверяемое поле с решённым (токен за токеном).
Ответь строго JSON без markdown:
{"finished": true|false, "reason": "краткое объяснение на русском"}`, board)

			raw, err := askLLMJSON(innerCtx, llm, model, prompt)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("LLM failed", err), nil
			}
			log.Printf("[CHECKER] is_finished %s", raw)
			return mcp.NewToolResultText(string(raw)), nil
		},
	)

	// check_is_valid — LLM проверяет ход и возвращает новую доску.
	s.AddTool(
		mcp.NewTool("check_is_valid",
			mcp.WithDescription(
				"Проверяет допустимость хода в «пятнашках». "+
					"LLM определяет, соседствует ли фишка с пустой клеткой, "+
					"и если да — возвращает новое состояние доски. "+
					"Возвращает {\"valid\": bool, \"board\": string, \"reason\": string}.",
			),
			mcp.WithString("board",
				mcp.Required(),
				mcp.Description("Текущая доска: 16 токенов через пробел."),
			),
			mcp.WithNumber("tile",
				mcp.Required(),
				mcp.Description("Номер фишки (1-15), которую нужно переместить."),
			),
		),
		func(innerCtx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			argsMap, _ := req.Params.Arguments.(map[string]any)
			if argsMap == nil {
				argsMap = map[string]any{}
			}
			board, _ := argsMap["board"].(string)
			tile := argsMap["tile"]

			prompt := fmt.Sprintf(`Ты — агент-проверяющий игры «пятнашки» (15-puzzle, 4×4).

Текущее поле (16 токенов через пробел, слева направо, сверху вниз, по 4 в строке):
%q

Игрок хочет переместить фишку номер %v на пустую клетку "_".

Правило: фишка может переместиться только если стоит непосредственно рядом
с пустой клеткой (сверху, снизу, слева или справа — без диагоналей).

Задачи:
1. Найди позицию фишки %v и позицию "_" на поле.
2. Проверь, являются ли они соседями по горизонтали или вертикали.
3. Если ход валиден — поменяй фишку и "_" местами и запиши новое поле.

Ответь строго JSON без markdown:
{
  "valid": true|false,
  "board": "<новое поле если valid=true, иначе — исходное поле без изменений>",
  "reason": "краткое объяснение на русском"
}`, board, tile, tile)

			raw, err := askLLMJSON(innerCtx, llm, model, prompt)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("LLM failed", err), nil
			}
			log.Printf("[CHECKER] check_is_valid %s", raw)
			return mcp.NewToolResultText(string(raw)), nil
		},
	)

	serveMCP(ctx, s, listenAddr, endpointPath)
}

// ── PLAYER ────────────────────────────────────────────────────────────────────
//
// Интерактивный REPL. Подключается к mcp-server по MCP-протоколу.
// Получает список инструментов через tools/list.
// Вызывает инструменты через MCP — сервер роутит к нужному агенту.

func connectMCP(ctx context.Context, mcpURL, name string) (*client.Client, error) {
	c, err := client.NewStreamableHttpClient(mcpURL)
	if err != nil {
		return nil, fmt.Errorf("new client: %w", err)
	}
	for attempt := 1; attempt <= 10; attempt++ {
		if ctx.Err() != nil {
			_ = c.Close()
			return nil, ctx.Err()
		}
		startCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		startErr := c.Start(startCtx)
		cancel()
		if startErr != nil {
			time.Sleep(800 * time.Millisecond)
			continue
		}
		initCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req := mcp.InitializeRequest{}
		req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
		req.Params.ClientInfo = mcp.Implementation{Name: name, Version: "1.0.0"}
		res, initErr := c.Initialize(initCtx, req)
		cancel()
		if initErr != nil {
			time.Sleep(800 * time.Millisecond)
			continue
		}
		log.Printf("mcp ok %s %s", mcpURL, res.ServerInfo.Name)
		return c, nil
	}
	_ = c.Close()
	return nil, errors.New("could not connect after 10 attempts")
}

func runPlayer(ctx context.Context, llm *openai.Client, model string) {
	mcpURL := firstNonEmpty(os.Getenv("GAME_SERVER_URL"), "http://mcp-server:9000/mcp")

	mcpClient, err := connectMCP(ctx, mcpURL, "player")
	if err != nil {
		log.Fatalf("MCP server: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	// Получаем список инструментов с сервера и конвертируем для OpenAI.
	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	toolsRes, err := mcpClient.ListTools(listCtx, mcp.ListToolsRequest{})
	cancel()
	if err != nil {
		log.Fatalf("ListTools: %v", err)
	}

	oaiTools := make([]openai.Tool, 0, len(toolsRes.Tools))
	for _, t := range toolsRes.Tools {
		schemaBytes, err := t.InputSchema.MarshalJSON()
		if err != nil {
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(schemaBytes, &params); err != nil {
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
	}

	// Все вызовы идут через MCP-клиент к mcp-server.
	// Сервер сам решает, к какому агенту (CREATOR / CHECKER) маршрутизировать.
	dispatch := func(ctx context.Context, toolName string, args map[string]any) (string, error) {
		callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		req := mcp.CallToolRequest{}
		req.Params.Name = toolName
		req.Params.Arguments = args
		res, err := mcpClient.CallTool(callCtx, req)
		if err != nil {
			return "", fmt.Errorf("MCP CallTool %s: %w", toolName, err)
		}
		var sb strings.Builder
		for _, item := range res.Content {
			if t, ok := mcp.AsTextContent(item); ok {
				sb.WriteString(t.Text)
			}
		}
		body := sb.String()
		if res.IsError {
			return "", fmt.Errorf("tool %s error: %s", toolName, body)
		}
		return body, nil
	}

	systemPrompt := `Игра «пятнашки» 4×4, цель: 1…15 и _ внизу справа. Инструменты: get_state, new_game, is_finished, move(tile). Роутер шлёт new_game в CREATOR, остальное проверки — в CHECKER. Если пользователь просит конкретный ход — вызови move.`

	runChatREPL(ctx, llm, model, oaiTools, systemPrompt, "PLAYER", dispatch, func(ctx context.Context, line string) bool {
		return playerTryDirectCommand(ctx, line, dispatch)
	})
}

func playerTryDirectCommand(ctx context.Context, line string, dispatch toolDispatcher) bool {
	trimmed := strings.TrimSpace(line)
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return false
	}
	cmd := strings.ToLower(strings.TrimPrefix(fields[0], "/"))

	out := func(tag string, res string, err error) {
		if err != nil {
			fmt.Printf("%s err: %v\n", tag, err)
		} else {
			fmt.Printf("%s %s\n", tag, res)
		}
	}

	switch cmd {
	case "help", "h", "?":
		fmt.Println("без LLM: /move N | /state | /new [тема] | /done | /?  ·  остальное → LLM  ·  /reset /exit")
		return true
	case "move", "m", "ход":
		if len(fields) < 2 {
			fmt.Println("usage: /move 1..15")
			return true
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil || n < 1 || n > 15 {
			fmt.Println("фишка 1..15")
			return true
		}
		log.Printf("player manual → CHECKER move(%d)", n)
		res, err := dispatch(ctx, "move", map[string]any{"tile": float64(n)})
		out("CHECKER", res, err)
		return true
	case "state", "board", "поле", "s":
		res, err := dispatch(ctx, "get_state", nil)
		out("board", res, err)
		return true
	case "new", "new_game", "игра":
		args := map[string]any{}
		if len(fields) > 1 {
			args["theme"] = strings.TrimSpace(strings.TrimPrefix(trimmed, fields[0]))
		}
		log.Printf("player manual → CREATOR new_game")
		res, err := dispatch(ctx, "new_game", args)
		out("CREATOR", res, err)
		return true
	case "finished", "check", "done", "win", "решено":
		log.Printf("player manual → CHECKER is_finished")
		res, err := dispatch(ctx, "is_finished", nil)
		out("CHECKER", res, err)
		return true
	}
	return false
}

// ── REPL ─────────────────────────────────────────────────────────────────────

type toolDispatcher func(ctx context.Context, toolName string, args map[string]any) (string, error)

func runChatREPL(
	ctx context.Context,
	llm *openai.Client,
	model string,
	tools []openai.Tool,
	systemPrompt string,
	prefix string,
	dispatch toolDispatcher,
	lineHook func(ctx context.Context, line string) bool,
) {
	var messages []openai.ChatCompletionMessage
	if systemPrompt != "" {
		messages = append(messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleSystem,
			Content: systemPrompt,
		})
	}

	fmt.Printf("%s готов. /move /state /new /done /? · /reset /exit\n", prefix)

	reader := bufio.NewReader(os.Stdin)
	for {
		select {
		case <-ctx.Done():
			fmt.Println()
			return
		default:
		}

		fmt.Printf("\nyou> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Println()
				return
			}
			log.Printf("stdin: %v", err)
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
			messages = messages[:1]
			fmt.Printf("%s: история очищена.\n", prefix)
			continue
		}

		if lineHook != nil && lineHook(ctx, line) {
			continue
		}

		messages = append(messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: line,
		})
		messages = runToolLoop(ctx, llm, model, tools, messages, prefix, dispatch)
	}
}

func runToolLoop(
	ctx context.Context,
	llm *openai.Client,
	model string,
	tools []openai.Tool,
	messages []openai.ChatCompletionMessage,
	prefix string,
	dispatch toolDispatcher,
) []openai.ChatCompletionMessage {
	const maxSteps = 20
	for step := 0; step < maxSteps; step++ {
		callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		resp, err := llm.CreateChatCompletion(callCtx, openai.ChatCompletionRequest{
			Model:       model,
			Temperature: 0.4,
			Messages:    messages,
			Tools:       tools,
		})
		cancel()
		if err != nil {
			log.Printf("LLM: %v", err)
			fmt.Printf("%s> [ошибка LLM: %v]\n", prefix, err)
			return messages
		}
		if len(resp.Choices) == 0 {
			fmt.Printf("%s> [пустой ответ модели]\n", prefix)
			return messages
		}

		msg := resp.Choices[0].Message
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
			fmt.Printf("%s> %s\n", prefix, content)
			return messages
		}

		for _, tc := range msg.ToolCalls {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			if args == nil {
				args = map[string]any{}
			}
			result, err := dispatch(ctx, tc.Function.Name, args)
			if err != nil {
				result = fmt.Sprintf(`{"error":%q}`, err.Error())
				log.Printf("llm %s err: %v", tc.Function.Name, err)
			} else {
				log.Printf("llm %s %s", tc.Function.Name, result)
			}

			messages = append(messages, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				Content:    result,
				ToolCallID: tc.ID,
			})
		}
	}

	log.Printf("tool loop exceeded maxSteps=%d", maxSteps)
	fmt.Printf("%s> [превышен лимит шагов]\n", prefix)
	return messages
}
