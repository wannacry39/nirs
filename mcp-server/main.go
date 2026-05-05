// MCP-сервер — центральный роутер и хранилище состояния.
//
// Инструменты для игрока (публичный MCP-эндпоинт /mcp):
//   get_state   — читает доску из памяти, без делегирования.
//   new_game    — делегирует агенту CREATOR по MCP (generate_new_game),
//                 сохраняет новую доску.
//   is_finished — делегирует агенту CHECKER по MCP (is_finished).
//   move        — делегирует агенту CHECKER по MCP (check_is_valid),
//                 сохраняет новую доску если ход валиден.
//
// Агенты CREATOR и CHECKER — полноценные MCP-серверы.
// Клиенты к ним живут в пакете ./clients.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"mcp-server/clients"
)

const (
	defaultListenAddr   = ":9000"
	defaultEndpointPath = "/mcp"
	solvedBoard         = "1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 _"
)

// ── хранилище ─────────────────────────────────────────────────────────────────

type store struct {
	mu    sync.RWMutex
	board string
}

func (s *store) get() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.board
}

func (s *store) set(board string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.board = board
}

// ── helpers ───────────────────────────────────────────────────────────────────

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ── MCP-сервер (инструменты для PLAYER) ──────────────────────────────────────

func buildMCPServer(st *store, creator *clients.CreatorClient, checker *clients.CheckerClient) *server.MCPServer {
	s := server.NewMCPServer(
		"puzzle-mcp-router",
		"2.0.0",
		server.WithToolCapabilities(false),
		server.WithRecovery(),
	)

	// get_state — локально, без делегирования.
	s.AddTool(
		mcp.NewTool("get_state",
			mcp.WithDescription("Текущая доска «пятнашки»: 16 токенов, числа 1–15 и _."),
		),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			board := st.get()
			log.Printf("state %q", board)
			out, _ := json.Marshal(map[string]string{"board": board})
			return mcp.NewToolResultText(string(out)), nil
		},
	)

	// new_game → CREATOR.generate_new_game
	s.AddTool(
		mcp.NewTool("new_game",
			mcp.WithDescription("Новая игра: CREATOR генерирует доску. theme — опционально."),
			mcp.WithString("theme", mcp.Description("Тема напутствия.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			argsMap, _ := req.Params.Arguments.(map[string]any)
			theme := ""
			if argsMap != nil {
				theme, _ = argsMap["theme"].(string)
			}
			result, err := creator.GenerateNewGame(ctx, theme)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("CREATOR", err), nil
			}
			if result.Board != "" {
				st.set(result.Board)
			}
			out, _ := json.Marshal(result)
			log.Printf("CREATOR generate_new_game %s", string(out))
			return mcp.NewToolResultText(string(out)), nil
		},
	)

	// is_finished → CHECKER.is_finished
	s.AddTool(
		mcp.NewTool("is_finished",
			mcp.WithDescription("Решена ли игра: CHECKER сравнивает доску с эталоном."),
		),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			board := st.get()
			result, err := checker.IsFinished(ctx, board)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("CHECKER", err), nil
			}
			out, _ := json.Marshal(result)
			log.Printf("CHECKER is_finished %s", string(out))
			return mcp.NewToolResultText(string(out)), nil
		},
	)

	// move → CHECKER.check_is_valid
	s.AddTool(
		mcp.NewTool("move",
			mcp.WithDescription("Ход: CHECKER валидирует; при успехе сервер сохраняет доску."),
			mcp.WithNumber("tile", mcp.Required(), mcp.Description("Фишка 1–15.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			argsMap, _ := req.Params.Arguments.(map[string]any)
			if argsMap == nil {
				out, _ := json.Marshal(map[string]any{"valid": false, "board": st.get(), "reason": "missing args"})
				return mcp.NewToolResultText(string(out)), nil
			}
			tileF, ok := argsMap["tile"].(float64)
			if !ok {
				out, _ := json.Marshal(map[string]any{"valid": false, "board": st.get(), "reason": "tile must be a number"})
				return mcp.NewToolResultText(string(out)), nil
			}

			board := st.get()
			result, err := checker.CheckIsValid(ctx, board, tileF)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("CHECKER", err), nil
			}
			if result.Valid && result.Board != "" && result.Board != board {
				st.set(result.Board)
			}
			out, _ := json.Marshal(result)
			log.Printf("CHECKER check_is_valid %s", string(out))
			return mcp.NewToolResultText(string(out)), nil
		},
	)

	return s
}

// ── main ─────────────────────────────────────────────────────────────────────

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("[mcp-server] ")
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	listenAddr := firstNonEmpty(os.Getenv("LISTEN_ADDR"), defaultListenAddr)
	endpointPath := firstNonEmpty(os.Getenv("ENDPOINT_PATH"), defaultEndpointPath)
	creatorURL := firstNonEmpty(os.Getenv("CREATOR_URL"), "http://creator:9001/mcp")
	checkerURL := firstNonEmpty(os.Getenv("CHECKER_URL"), "http://checker:9002/mcp")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Подключаемся к агентам через MCP-клиенты.
	creator, err := clients.NewCreatorClient(ctx, creatorURL)
	if err != nil {
		log.Fatalf("CREATOR: %v", err)
	}
	defer func() { _ = creator.Close() }()

	checker, err := clients.NewCheckerClient(ctx, checkerURL)
	if err != nil {
		log.Fatalf("CHECKER: %v", err)
	}
	defer func() { _ = checker.Close() }()

	st := &store{board: solvedBoard}
	mcpSrv := buildMCPServer(st, creator, checker)

	mcpHandler := server.NewStreamableHTTPServer(mcpSrv,
		server.WithEndpointPath(endpointPath),
	)
	mux := http.NewServeMux()
	mux.Handle(endpointPath, mcpHandler)
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
		log.Printf("listening on %s%s  [CREATOR=%s  CHECKER=%s]",
			listenAddr, endpointPath, creatorURL, checkerURL)
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
