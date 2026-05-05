// Package clients содержит MCP-клиенты к агентам CREATOR и CHECKER.
// mcp-server использует их для делегирования запросов игрока нужному агенту.
package clients

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// connectMCP устанавливает MCP-соединение с агентом по заданному URL.
// Делает до 10 попыток с паузой — сглаживает race при запуске контейнеров.
func connectMCP(ctx context.Context, url, name string) (*client.Client, error) {
	log.Printf("[clients] connecting to %s at %s", name, url)
	c, err := client.NewStreamableHttpClient(url)
	if err != nil {
		return nil, fmt.Errorf("new client %s: %w", name, err)
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
			log.Printf("[clients] %s start attempt %d/10: %v", name, attempt, startErr)
			time.Sleep(800 * time.Millisecond)
			continue
		}
		initCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req := mcp.InitializeRequest{}
		req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
		req.Params.ClientInfo = mcp.Implementation{Name: "mcp-server", Version: "2.0.0"}
		res, initErr := c.Initialize(initCtx, req)
		cancel()
		if initErr != nil {
			log.Printf("[clients] %s init attempt %d/10: %v", name, attempt, initErr)
			time.Sleep(800 * time.Millisecond)
			continue
		}
		log.Printf("[clients] connected to %s (%s v%s)", name, res.ServerInfo.Name, res.ServerInfo.Version)
		return c, nil
	}
	_ = c.Close()
	return nil, errors.New("could not connect to " + name + " after 10 attempts")
}

// callTool вызывает инструмент на агенте и возвращает текст ответа.
func callTool(ctx context.Context, c *client.Client, tool string, args map[string]any) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	req := mcp.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args

	res, err := c.CallTool(callCtx, req)
	if err != nil {
		return "", fmt.Errorf("CallTool %s: %w", tool, err)
	}

	var sb strings.Builder
	for _, item := range res.Content {
		if t, ok := mcp.AsTextContent(item); ok {
			sb.WriteString(t.Text)
		}
	}
	body := sb.String()
	if res.IsError {
		return "", fmt.Errorf("tool %s error: %s", tool, body)
	}
	return body, nil
}
