// Package llm is a minimal streaming client for OpenAI-compatible
// chat completions endpoints. No external dependencies.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Message is a chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client talks to one OpenAI-compatible endpoint.
type Client struct {
	BaseURL string // e.g. http://host:1234/v1
	APIKey  string
	Model   string
	HTTP    *http.Client

	// Extra fields merged into the request body (server-specific
	// flags such as enable_thinking).
	Extra map[string]any

	// Protocol: "" or "openai" (default) | "anthropic".
	Protocol string
}

func New(baseURL, apiKey, model string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{},
	}
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// Reasoning traces from thinking models. Server field names
			// vary: reasoning_content (OpenAI/DeepSeek style) vs
			// reasoning (Ollama).
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
		} `json:"delta"`
	} `json:"choices"`
}

// ChatStream streams a completion, invoking onDelta for each content
// delta and onThinking for each reasoning delta (either may be nil).
// It returns after the stream completes or ctx is canceled.
func (c *Client) ChatStream(
	ctx context.Context,
	msgs []Message,
	onDelta func(string),
	onThinking func(string),
) error {
	if c.Protocol == "anthropic" {
		return c.chatStreamAnthropic(ctx, msgs, onDelta, onThinking)
	}
	raw, err := json.Marshal(chatRequest{
		Model:    c.Model,
		Messages: msgs,
		Stream:   true,
	})
	if err != nil {
		return err
	}

	// Merge provider-specific flags on top of the standard body.
	var bodyMap map[string]any
	if err := json.Unmarshal(raw, &bodyMap); err != nil {
		return err
	}
	for k, v := range c.Extra {
		bodyMap[k] = v
	}
	body, err := json.Marshal(bodyMap)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.BaseURL+"/chat/completions",
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("provider request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("provider returned %s: %s",
			resp.Status, strings.TrimSpace(string(msg)))
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return nil
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Be forgiving of non-JSON keepalive lines.
			continue
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" && onDelta != nil {
				onDelta(ch.Delta.Content)
			}
			if onThinking != nil {
				think := ch.Delta.ReasoningContent
				if think == "" {
					think = ch.Delta.Reasoning
				}
				if think != "" {
					onThinking(think)
				}
			}
		}
	}
	if ctx.Err() == context.Canceled {
		return nil // cancellation is a clean stop
	}
	return sc.Err()
}

// ---------------------------------------------------------------------------
// Anthropic native Messages API
// ---------------------------------------------------------------------------

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicDelta struct {
	Type  string `json:"type"`
	Delta struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"delta"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) chatStreamAnthropic(
	ctx context.Context,
	msgs []Message,
	onDelta func(string),
	onThinking func(string),
) error {
	var system []string
	var flat []anthropicMessage
	for _, m := range msgs {
		if m.Role == "system" {
			system = append(system, m.Content)
			continue
		}
		// Anthropic insists on alternating roles; our history grows
		// consecutive user turns (exec results, scrollback notes),
		// so merge those into the previous message.
		if n := len(flat); n > 0 && flat[n-1].Role == m.Role {
			flat[n-1].Content += "\n\n" + m.Content
			continue
		}
		flat = append(flat, anthropicMessage{Role: m.Role, Content: m.Content})
	}
	if len(flat) == 0 || flat[0].Role != "user" {
		flat = append([]anthropicMessage{{Role: "user", Content: "(continue)"}}, flat...)
	}

	bodyMap := map[string]any{
		"model":      c.Model,
		"messages":   flat,
		"stream":     true,
		"max_tokens": 8192,
	}
	if len(system) > 0 {
		bodyMap["system"] = strings.Join(system, "\n\n")
	}
	for k, v := range c.Extra {
		bodyMap[k] = v
	}
	body, err := json.Marshal(bodyMap)
	if err != nil {
		return err
	}

	url := strings.TrimSuffix(c.BaseURL, "/")
	url = strings.TrimSuffix(url, "/v1") + "/v1/messages"

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, url, bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if c.APIKey != "" {
		req.Header.Set("x-api-key", c.APIKey)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("provider request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("provider returned %s: %s",
			resp.Status, strings.TrimSpace(string(msg)))
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue // "event:" lines and keepalives
		}
		var ev anthropicDelta
		if err := json.Unmarshal([]byte(strings.TrimSpace(
			strings.TrimPrefix(line, "data:"))), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" && onDelta != nil {
					onDelta(ev.Delta.Text)
				}
			case "thinking_delta":
				if ev.Delta.Thinking != "" && onThinking != nil {
					onThinking(ev.Delta.Thinking)
				}
			}
		case "error":
			return fmt.Errorf("anthropic stream error (%s): %s",
				ev.Error.Type, ev.Error.Message)
		case "message_stop":
			return nil
		}
	}
	return sc.Err()
}
