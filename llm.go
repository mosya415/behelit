package main

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

// Message is a single chat-completions message. We keep a running transcript of
// these; tool results are appended as plain user messages (we do not rely on the
// native tool/function-calling channel — see protocol.go).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client is a tiny OpenAI-compatible chat-completions client. stdlib only.
type Client struct {
	http      *http.Client
	baseURL   string
	endpoints []string // known endpoints, for /endpoint switching
	model     string
	apiKey    string
	temp      float64
}

func NewClient(cfg Config) *Client {
	eps := cfg.Endpoints
	if len(eps) == 0 {
		eps = []string{cfg.BaseURL}
	}
	return &Client{
		http:      &http.Client{Timeout: 10 * time.Minute},
		baseURL:   cfg.BaseURL,
		endpoints: eps,
		model:     cfg.Model,
		apiKey:    cfg.APIKey,
		temp:      cfg.Temperature,
	}
}

func (c *Client) Endpoints() []string { return c.endpoints }

// SetEndpoint switches the active endpoint (trailing slash trimmed, http://
// prepended when no scheme is given) and remembers it in the known list.
func (c *Client) SetEndpoint(u string) {
	u = strings.TrimRight(u, "/")
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	c.baseURL = u
	for _, e := range c.endpoints {
		if e == u {
			return
		}
	}
	c.endpoints = append(c.endpoints, u)
}

func (c *Client) Model() string     { return c.model }
func (c *Client) SetModel(m string) { c.model = m }
func (c *Client) Endpoint() string  { return c.baseURL }

// ModelInfo is what we surface about a served model. Fields beyond ID are
// best-effort: vLLM populates owned_by and max_model_len; leaner servers (e.g.
// SGLang) may omit them, in which case they read as empty/0 and are hidden.
type ModelInfo struct {
	ID      string
	OwnedBy string
	MaxLen  int // context window (max_model_len), 0 if unknown
}

type modelsResponse struct {
	Data []struct {
		ID          string `json:"id"`
		OwnedBy     string `json:"owned_by"`
		MaxModelLen int    `json:"max_model_len"`
	} `json:"data"`
}

// ListModels queries the OpenAI-compatible /models endpoint to discover what the
// server actually serves. Short timeout so a hung or absent endpoint never
// blocks startup. Not every server implements it — callers treat an error as
// "discovery unavailable", not fatal.
func (c *Client) ListModels() ([]ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("endpoint returned %d", resp.StatusCode)
	}
	var out modelsResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("bad /models json: %w", err)
	}
	infos := make([]ModelInfo, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			infos = append(infos, ModelInfo{ID: m.ID, OwnedBy: m.OwnedBy, MaxLen: m.MaxModelLen})
		}
	}
	return infos, nil
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature"`
	Stream      bool      `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends the transcript and returns the assistant's raw text. No
// streaming in this cut — keeps the loop trivial and the parser total.
func (c *Client) Complete(msgs []Message) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    msgs,
		Temperature: c.temp,
		Stream:      false,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("request to %s failed: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("endpoint returned %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("bad response json: %w", err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("endpoint error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("endpoint returned no choices")
	}
	return out.Choices[0].Message.Content, nil
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

// CompleteStream streams the assistant reply over SSE, calling onDelta for each
// text fragment as it arrives, and returns the fully assembled text. We parse
// the token stream ourselves; the tool-call protocol only sees the final text,
// so streaming is purely a UX layer and cannot affect correctness.
func (c *Client) CompleteStream(msgs []Message, onDelta func(string)) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    msgs,
		Temperature: c.temp,
		Stream:      true,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("request to %s failed: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("endpoint returned %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}

	reader := bufio.NewReader(resp.Body)
	var sb strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "data:") {
			data := strings.TrimSpace(s[len("data:"):])
			if data == "[DONE]" {
				break
			}
			var chunk streamChunk
			if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 {
				if d := chunk.Choices[0].Delta.Content; d != "" {
					sb.WriteString(d)
					onDelta(d)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return sb.String(), err
		}
	}
	return sb.String(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
