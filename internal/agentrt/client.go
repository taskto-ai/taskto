package agentrt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type Client struct {
	http *http.Client
}

func New(socketPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{http: &http.Client{Transport: transport}}
}

type Input struct {
	Content    string `json:"content"`
	WhenBusy   string `json:"whenBusy,omitempty"`
	RequestID  string `json:"requestId,omitempty"`
	Capability string `json:"capability,omitempty"`
	Language   string `json:"language"`
}

type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type Settled struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type ModelRef struct {
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

type Catalog struct {
	Providers []Provider `json:"providers"`
	Default   ModelRef   `json:"default"`
}

type Provider struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Kind   string      `json:"kind"`
	Models []ModelInfo `json:"models"`
}

type ModelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type StatusError struct {
	Code    int
	Status  string
	Message string
	Reason  string
}

func (e *StatusError) Error() string { return fmt.Sprintf("agentrt: %s: %s", e.Status, e.Message) }

type Event struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value,omitempty"`
	Ops   json.RawMessage `json:"ops,omitempty"`
	Kind  string          `json:"kind,omitempty"`
	Raw   json.RawMessage `json:"-"`
}

func (c *Client) Healthy(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/healthz", nil, nil)
}

type Health struct {
	Status string `json:"status"`
	PID    int    `json:"pid"`
}

func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.do(ctx, http.MethodGet, "/healthz", nil, &h)
	return h, err
}

func (c *Client) Models(ctx context.Context) (Catalog, error) {
	var out Catalog
	err := c.do(ctx, http.MethodGet, "/models", nil, &out)
	return out, err
}

func (c *Client) ReloadProviders(ctx context.Context) (Catalog, error) {
	var out Catalog
	err := c.do(ctx, http.MethodPost, "/providers/reload", nil, &out)
	return out, err
}

func (c *Client) CreateConversation(ctx context.Context) (int64, error) {
	return c.CreateConversationOn(ctx, ModelRef{})
}

func (c *Client) CreateConversationOn(ctx context.Context, model ModelRef) (int64, error) {
	var in struct {
		Model *ModelRef `json:"model,omitempty"`
	}
	if model != (ModelRef{}) {
		in.Model = &model
	}
	var out struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, "/conversations", in, &out)
	return out.ID, err
}

type ConversationInfo struct {
	ID        int64 `json:"id"`
	CreatedAt int64 `json:"createdAt"`
	Retired   bool  `json:"retired"`
}

func (c *Client) Conversations(ctx context.Context) ([]ConversationInfo, error) {
	var out struct {
		Conversations []ConversationInfo `json:"conversations"`
	}
	err := c.do(ctx, http.MethodGet, "/conversations", nil, &out)
	return out.Conversations, err
}

func (c *Client) RetireConversation(ctx context.Context, conversation int64) error {
	return c.do(ctx, http.MethodPost, "/conversations/"+itoa(conversation)+"/retire", nil, nil)
}

func (c *Client) PurgeConversation(ctx context.Context, conversation int64) error {
	return c.do(ctx, http.MethodPost, "/conversations/"+itoa(conversation)+"/purge", nil, nil)
}

func (c *Client) Model(ctx context.Context, conversation int64) (ModelRef, error) {
	var out ModelRef
	err := c.do(ctx, http.MethodGet, "/conversations/"+itoa(conversation)+"/model", nil, &out)
	return out, err
}

func (c *Client) SetModel(ctx context.Context, conversation int64, model ModelRef) error {
	return c.do(ctx, http.MethodPut, "/conversations/"+itoa(conversation)+"/model", model, nil)
}

func (c *Client) Submit(ctx context.Context, conversation int64, in Input) (int64, error) {
	var out struct {
		SubmissionID int64 `json:"submissionId"`
	}
	err := c.do(ctx, http.MethodPost, "/conversations/"+itoa(conversation)+"/submit", in, &out)
	return out.SubmissionID, err
}

func (c *Client) Continue(ctx context.Context, conversation int64, model ModelRef, afterEntry int64) (int64, error) {
	var out struct {
		SubmissionID int64 `json:"submissionId"`
	}
	in := struct {
		Model      ModelRef `json:"model"`
		AfterEntry int64    `json:"afterEntry"`
	}{model, afterEntry}
	err := c.do(ctx, http.MethodPost, "/conversations/"+itoa(conversation)+"/continue", in, &out)
	return out.SubmissionID, err
}

func (c *Client) Wait(ctx context.Context, submission int64) (Settled, error) {
	var out Settled
	err := c.do(ctx, http.MethodPost, "/submissions/"+itoa(submission)+"/wait", nil, &out)
	return out, err
}

func (c *Client) Messages(ctx context.Context, conversation int64) ([]Message, error) {
	var out []Message
	err := c.do(ctx, http.MethodGet, "/conversations/"+itoa(conversation)+"/messages", nil, &out)
	return out, err
}

func (c *Client) Abort(ctx context.Context, conversation int64) error {
	return c.do(ctx, http.MethodPost, "/conversations/"+itoa(conversation)+"/abort", nil, nil)
}

func (c *Client) Entries(ctx context.Context, conversation, before int64, limit int) (json.RawMessage, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if before > 0 {
		q.Set("before", itoa(before))
	}
	var out json.RawMessage
	err := c.do(ctx, http.MethodGet, "/conversations/"+itoa(conversation)+"/entries?"+q.Encode(), nil, &out)
	return out, err
}

func (c *Client) Watch(ctx context.Context, conversation int64) (*Watch, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent/conversations/"+itoa(conversation)+"/watch", nil)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		return nil, statusError(res)
	}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 64*1024), maxWatchLine)
	return &Watch{body: res.Body, scanner: scanner}, nil
}

const maxWatchLine = 16 << 20

var ErrWatchLineTooLong = errors.New("agentrt: watch line over 16 MB")

var ErrWatchMalformed = errors.New("agentrt: malformed watch line")

type Watch struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
}

func (w *Watch) Next() (Event, error) {
	if !w.scanner.Scan() {
		_ = w.body.Close()
		if err := w.scanner.Err(); errors.Is(err, bufio.ErrTooLong) {
			return Event{}, ErrWatchLineTooLong
		} else if err != nil {
			return Event{}, err
		}
		return Event{}, io.EOF
	}
	var ev Event
	if err := json.Unmarshal(w.scanner.Bytes(), &ev); err != nil {
		_ = w.body.Close()
		return Event{}, fmt.Errorf("%w: %w", ErrWatchMalformed, err)
	}
	ev.Raw = bytes.Clone(w.scanner.Bytes())
	return ev, nil
}

func (w *Watch) Close() error { return w.body.Close() }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return statusError(res)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, res.Body)
		return err
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func statusError(res *http.Response) error {
	var e struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 64*1024)).Decode(&e)
	return &StatusError{Code: res.StatusCode, Status: res.Status, Message: e.Error, Reason: e.Reason}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }
