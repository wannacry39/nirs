package clients

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/client"
)

// CheckerClient — MCP-клиент к агенту CHECKER.
// Агент предоставляет инструменты: is_finished, check_is_valid.
type CheckerClient struct {
	c *client.Client
}

// NewCheckerClient подключается к MCP-серверу агента CHECKER.
func NewCheckerClient(ctx context.Context, url string) (*CheckerClient, error) {
	c, err := connectMCP(ctx, url, "CHECKER")
	if err != nil {
		return nil, err
	}
	return &CheckerClient{c: c}, nil
}

func (cc *CheckerClient) Close() error {
	return cc.c.Close()
}

// IsFinishedResult — ответ инструмента is_finished.
type IsFinishedResult struct {
	Finished bool   `json:"finished"`
	Reason   string `json:"reason"`
}

// IsFinished вызывает инструмент is_finished у агента CHECKER.
// Агент через LLM определяет, совпадает ли доска с решённым состоянием.
func (cc *CheckerClient) IsFinished(ctx context.Context, board string) (*IsFinishedResult, error) {
	raw, err := callTool(ctx, cc.c, "is_finished", map[string]any{"board": board})
	if err != nil {
		return nil, fmt.Errorf("is_finished: %w", err)
	}
	var result IsFinishedResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("decode IsFinishedResult: %w (raw=%s)", err, raw)
	}
	return &result, nil
}

// CheckMoveResult — ответ инструмента check_is_valid.
type CheckMoveResult struct {
	Valid  bool   `json:"valid"`
	Board  string `json:"board"`
	Reason string `json:"reason"`
}

// CheckIsValid вызывает инструмент check_is_valid у агента CHECKER.
// Агент через LLM проверяет допустимость хода и возвращает новую доску если ход валиден.
func (cc *CheckerClient) CheckIsValid(ctx context.Context, board string, tile float64) (*CheckMoveResult, error) {
	raw, err := callTool(ctx, cc.c, "check_is_valid", map[string]any{
		"board": board,
		"tile":  tile,
	})
	if err != nil {
		return nil, fmt.Errorf("check_is_valid: %w", err)
	}
	var result CheckMoveResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("decode CheckMoveResult: %w (raw=%s)", err, raw)
	}
	return &result, nil
}
