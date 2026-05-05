package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/mark3labs/mcp-go/client"
)

// CreatorClient — MCP-клиент к агенту CREATOR.
// Агент предоставляет инструмент: generate_new_game.
type CreatorClient struct {
	c *client.Client
}

// NewCreatorClient подключается к MCP-серверу агента CREATOR.
func NewCreatorClient(ctx context.Context, url string) (*CreatorClient, error) {
	c, err := connectMCP(ctx, url, "CREATOR")
	if err != nil {
		return nil, err
	}
	return &CreatorClient{c: c}, nil
}

func (cc *CreatorClient) Close() error {
	return cc.c.Close()
}

// NewGameResult — ответ инструмента generate_new_game.
type NewGameResult struct {
	Board   string `json:"board"`
	Message string `json:"message"`
}

// GenerateNewGame вызывает инструмент generate_new_game у агента CREATOR.
// Возвращает перемешанную доску и напутствие от LLM.
func (cc *CreatorClient) GenerateNewGame(ctx context.Context, theme string) (*NewGameResult, error) {
	args := map[string]any{}
	if theme != "" {
		args["theme"] = theme
	}
	raw, err := callTool(ctx, cc.c, "generate_new_game", args)
	if err != nil {
		return nil, fmt.Errorf("generate_new_game: %w", err)
	}
	log.Printf("CREATOR %s", raw)
	var result NewGameResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("decode NewGameResult: %w (raw=%s)", err, raw)
	}
	return &result, nil
}
