// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	agentapi "mautrix-android-sms-mms/internal/agent"
)

// AgentClient is a minimal HTTP client for the android-sms-mms-adb-agent API
// described in docs/AGENT_API.md. All methods are safe for concurrent use.
type AgentClient struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewAgentClient creates a client for the given base URL and bearer token.
func NewAgentClient(baseURL, token string) *AgentClient {
	return &AgentClient{
		BaseURL: normalizeAgentURL(baseURL),
		Token:   strings.TrimSpace(token),
		HTTP:    &http.Client{Timeout: 35 * time.Second},
	}
}

// normalizeAgentURL strips trailing slashes and adds an http:// scheme when
// the user typed a bare host:port.
func normalizeAgentURL(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL != "" && !strings.Contains(baseURL, "://") {
		baseURL = "http://" + baseURL
	}
	return baseURL
}

// APIError is a structured error returned by the agent ({"error","message"}).
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e == nil {
		return "agent API error"
	}
	if e.Message != "" {
		return fmt.Sprintf("agent API error %d (%s): %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("agent API error %d (%s)", e.Status, e.Code)
}

// SendAttachment is an outgoing MMS attachment (base64 encoded).
type SendAttachment struct {
	ContentType string `json:"content_type"`
	FileName    string `json:"file_name,omitempty"`
	Base64      string `json:"base64"`
}

// SendRequest is the POST /v1/send body.
type SendRequest struct {
	SendID      string           `json:"send_id"`
	ThreadID    int64            `json:"thread_id,omitempty"`
	SubID       int              `json:"sub_id,omitempty"`
	Recipients  []string         `json:"recipients,omitempty"`
	Text        string           `json:"text,omitempty"`
	Attachments []SendAttachment `json:"attachments,omitempty"`
}

// SendResponse is the POST /v1/send response.
type SendResponse struct {
	SendID    string `json:"send_id"`
	MessageID string `json:"message_id"`
	ThreadID  int64  `json:"thread_id"`
	PartsSent int    `json:"parts_sent"`
	Pending   bool   `json:"pending"`
}

// ReadResponse is the POST /v1/read response.
type ReadResponse struct {
	Updated   int  `json:"updated"`
	Supported bool `json:"supported"`
}

// EventsResponse is the GET /v1/events response.
type EventsResponse struct {
	Reset  bool             `json:"reset"`
	Cursor int64            `json:"cursor"`
	Events []agentapi.Event `json:"events"`
}

// Health fetches the agent /v1/health payload.
func (c *AgentClient) Health(ctx context.Context) (agentapi.Health, error) {
	var health agentapi.Health
	err := c.do(ctx, http.MethodGet, "/v1/health", nil, &health)
	return health, err
}

// Subscriptions fetches the SIM list.
func (c *AgentClient) Subscriptions(ctx context.Context) ([]agentapi.Subscription, error) {
	var subs []agentapi.Subscription
	err := c.do(ctx, http.MethodGet, "/v1/subscriptions", nil, &subs)
	return subs, err
}

// Threads fetches all telephony threads.
func (c *AgentClient) Threads(ctx context.Context) ([]agentapi.Thread, error) {
	var threads []agentapi.Thread
	err := c.do(ctx, http.MethodGet, "/v1/threads", nil, &threads)
	return threads, err
}

// ThreadMessages fetches a page of messages in a thread. beforeMS/afterMS are
// Unix millisecond bounds; zero means unbounded. Messages are returned oldest
// first.
func (c *AgentClient) ThreadMessages(ctx context.Context, threadID int64, beforeMS, afterMS int64, limit int) ([]agentapi.Message, error) {
	query := url.Values{}
	if beforeMS > 0 {
		query.Set("before_ms", strconv.FormatInt(beforeMS, 10))
	}
	if afterMS > 0 {
		query.Set("after_ms", strconv.FormatInt(afterMS, 10))
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	path := fmt.Sprintf("/v1/threads/%d/messages", threadID)
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var msgs []agentapi.Message
	err := c.do(ctx, http.MethodGet, path, nil, &msgs)
	return msgs, err
}

// Message fetches a single message by its agent message ID.
func (c *AgentClient) Message(ctx context.Context, id string) (agentapi.Message, error) {
	var msg agentapi.Message
	err := c.do(ctx, http.MethodGet, "/v1/messages/"+url.PathEscape(id), nil, &msg)
	return msg, err
}

// PartBytes downloads one MMS part and returns the bytes plus the
// Content-Type reported by the agent.
func (c *AgentClient) PartBytes(ctx context.Context, mmsID, partID int64) ([]byte, string, error) {
	path := fmt.Sprintf("/v1/mms/%d/parts/%d", mmsID, partID)
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("request %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", decodeAPIError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024*1024))
	if err != nil {
		return nil, "", fmt.Errorf("read part %d of mms %d: %w", partID, mmsID, err)
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// Send sends an SMS or MMS.
func (c *AgentClient) Send(ctx context.Context, reqBody SendRequest) (SendResponse, error) {
	var resp SendResponse
	err := c.do(ctx, http.MethodPost, "/v1/send", reqBody, &resp)
	return resp, err
}

// Read marks a thread (or specific messages) as read on the phone.
func (c *AgentClient) Read(ctx context.Context, threadID, upToMS int64, messageIDs []string) (ReadResponse, error) {
	body := map[string]any{}
	if threadID != 0 {
		body["thread_id"] = threadID
		body["up_to_ms"] = upToMS
	}
	if len(messageIDs) > 0 {
		body["message_ids"] = messageIDs
	}
	var resp ReadResponse
	err := c.do(ctx, http.MethodPost, "/v1/read", body, &resp)
	return resp, err
}

// Events long-polls the agent event feed. timeoutMS is in milliseconds; the
// agent clamps it to 25000.
func (c *AgentClient) Events(ctx context.Context, since int64, timeoutMS int) (EventsResponse, error) {
	query := url.Values{}
	query.Set("since", strconv.FormatInt(since, 10))
	if timeoutMS > 0 {
		query.Set("timeout_ms", strconv.Itoa(timeoutMS))
	}
	var resp EventsResponse
	err := c.do(ctx, http.MethodGet, "/v1/events?"+query.Encode(), nil, &resp)
	return resp, err
}

func (c *AgentClient) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *AgentClient) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	if strings.TrimSpace(c.BaseURL) == "" {
		return nil, fmt.Errorf("agent URL is not configured")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request body: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("create request %s: %w", path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return req, nil
}

func (c *AgentClient) do(ctx context.Context, method, path string, body any, out any) error {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeAPIError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response of %s: %w", path, err)
	}
	return nil
}

func decodeAPIError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var envelope struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &envelope)
	code := strings.TrimSpace(envelope.Error)
	message := strings.TrimSpace(envelope.Message)
	if code == "" {
		code = strings.ToLower(http.StatusText(resp.StatusCode))
		if code == "" {
			code = "unknown"
		}
	}
	return &APIError{Status: resp.StatusCode, Code: code, Message: message}
}
