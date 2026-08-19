package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	baseURL, token string
	http           *http.Client
}

func NewClient(token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 70 * time.Second}
	}
	return &Client{baseURL: "https://api.telegram.org", token: token, http: httpClient}
}
func (c *Client) call(ctx context.Context, method string, body any, out any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+url.PathEscape(c.token)+"/"+method, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram HTTP %d", resp.StatusCode)
	}
	var envelope APIResponse[json.RawMessage]
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	if !envelope.OK {
		return fmt.Errorf("telegram API %d: %s", envelope.ErrorCode, envelope.Description)
	}
	if out != nil && len(envelope.Result) > 0 {
		return json.Unmarshal(envelope.Result, out)
	}
	return nil
}
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var out User
	err := c.call(ctx, "getMe", nil, &out)
	return out, err
}
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	params := map[string]any{"offset": offset, "timeout": timeout, "allowed_updates": []string{"message", "callback_query"}}
	var out []Update
	err := c.call(ctx, "getUpdates", params, &out)
	return out, err
}
func (c *Client) SendMessage(ctx context.Context, p SendMessageParams) (Message, error) {
	var out Message
	err := c.call(ctx, "sendMessage", p, &out)
	return out, err
}
func (c *Client) EditMessage(ctx context.Context, p EditMessageParams) (Message, error) {
	var out Message
	err := c.call(ctx, "editMessageText", p, &out)
	return out, err
}
func (c *Client) AnswerCallback(ctx context.Context, p CallbackAnswerParams) error {
	return c.call(ctx, "answerCallbackQuery", p, nil)
}
func (c *Client) SetMessageReaction(ctx context.Context, p SetMessageReactionParams) error {
	return c.call(ctx, "setMessageReaction", p, nil)
}
func (c *Client) CreateForumTopic(ctx context.Context, p CreateForumTopicParams) (ForumTopic, error) {
	var out ForumTopic
	err := c.call(ctx, "createForumTopic", p, &out)
	return out, err
}
func (c *Client) SetBaseURL(base string) { c.baseURL = strings.TrimRight(base, "/") }
func ChatIDString(id int64) string       { return strconv.FormatInt(id, 10) }
