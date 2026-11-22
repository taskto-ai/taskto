// Package agentrt talks to the agent runtime sidecar (agent/) over its Unix socket.
//
// The sidecar has no auth of its own: callers in this server must check who may read or
// steer a conversation before using this client.
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

// Client is a connection to one sidecar.
type Client struct {
	http *http.Client
}

// New returns a client for the sidecar listening on socketPath.
//
// Only connecting has a fixed timeout. Wait and Watch legitimately last as long as a run, so
// every call takes its deadline from ctx, and callers must pass one.
func New(socketPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{http: &http.Client{Transport: transport}}
}

// Input is a user message for a conversation.
type Input struct {
	Content string `json:"content"`
	// WhenBusy is "steer", "followUp" (default) or "reject".
	WhenBusy string `json:"whenBusy,omitempty"`
	// RequestID makes a retried submit return the original submission.
	RequestID string `json:"requestId,omitempty"`
	// Capability is the conversation's capability for the tools socket
	// (SupervisorConfig.Tools), which the sidecar keeps for the conversation's tool calls.
	Capability string `json:"capability,omitempty"`
	// Language is the BCP 47 tag of the language the agent replies in by default, or "" when
	// none is known: it then replies in the language the user writes in. Every message states
	// it, so a change reaches the conversation with the next one.
	Language string `json:"language"`
}

// Message is one message of the model's current context, simplified to role and text.
type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// Settled is how a submission ended: Status "done" or "unanswered".
type Settled struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// ModelRef names one configured model of one provider.
type ModelRef struct {
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

// Catalog is the sidecar's configured providers and models. It holds no endpoints or keys.
type Catalog struct {
	Providers []Provider `json:"providers"`
	// Default is the model a conversation gets when none is chosen.
	Default ModelRef `json:"default"`
}

// Provider is one configured provider. Kind is "openai-compatible" or "script".
type Provider struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Kind   string      `json:"kind"`
	Models []ModelInfo `json:"models"`
}

// ModelInfo is one model of a provider.
type ModelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// StatusError is a response the sidecar refused, such as 400 for an unknown model or 404
// for an unknown conversation.
type StatusError struct {
	Code    int
	Status  string
	Message string
	// Reason is the sidecar's stable word for a refusal people can cause: "unknown_model",
	// "no_model", or "" (agent/src/index.ts, HttpError).
	Reason string
}

func (e *StatusError) Error() string { return fmt.Sprintf("agentrt: %s: %s", e.Status, e.Message) }

// Event is one line of a watch stream. Type "value" carries the whole view, "ops" the Chord
// operations of the commits since the last line, and "end" says why the stream ended.
type Event struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value,omitempty"`
	Ops   json.RawMessage `json:"ops,omitempty"`
	Kind  string          `json:"kind,omitempty"`
	// Raw is the line as the sidecar sent it, without the newline, for passing it on unchanged.
	Raw json.RawMessage `json:"-"`
}

// Healthy reports whether the sidecar answers.
func (c *Client) Healthy(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/healthz", nil, nil)
}

// Health is the sidecar's /healthz answer.
type Health struct {
	Status string `json:"status"`
	// PID is the process answering, so a supervisor can tell its own sidecar from another.
	PID int `json:"pid"`
}

// Health returns the sidecar's health, including which process answered.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.do(ctx, http.MethodGet, "/healthz", nil, &h)
	return h, err
}

// Models lists the configured providers and models.
func (c *Client) Models(ctx context.Context) (Catalog, error) {
	var out Catalog
	err := c.do(ctx, http.MethodGet, "/models", nil, &out)
	return out, err
}

// ReloadProviders makes the sidecar read its providers file again and returns the new catalog.
// Conversations keep running. A file the sidecar refuses is a *StatusError with Code 400, and
// the sidecar keeps the providers it had.
func (c *Client) ReloadProviders(ctx context.Context) (Catalog, error) {
	var out Catalog
	err := c.do(ctx, http.MethodPost, "/providers/reload", nil, &out)
	return out, err
}

// CreateConversation creates a conversation on the default model and returns its ID.
func (c *Client) CreateConversation(ctx context.Context) (int64, error) {
	return c.CreateConversationOn(ctx, ModelRef{})
}

// CreateConversationOn creates a conversation on model, or on the default model if model is
// the zero ModelRef. A model that isn't configured is a *StatusError with Code 400.
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

// ConversationInfo is one conversation the sidecar has.
type ConversationInfo struct {
	ID int64 `json:"id"`
	// CreatedAt is when it was created, in milliseconds since the epoch; 0 if it was created
	// by a sidecar that didn't record it yet.
	CreatedAt int64 `json:"createdAt"`
	// Retired is true once RetireConversation retired it: every other call then answers 404.
	Retired bool `json:"retired"`
}

// Conversations lists every conversation created through CreateConversation, retired ones
// included, oldest first.
func (c *Client) Conversations(ctx context.Context) ([]ConversationInfo, error) {
	var out struct {
		Conversations []ConversationInfo `json:"conversations"`
	}
	err := c.do(ctx, http.MethodGet, "/conversations", nil, &out)
	return out.Conversations, err
}

// RetireConversation stops a conversation's work and retires it: from then on the sidecar
// answers 404 for it, as for an unknown one. Pi Durable never deletes a conversation, so its
// records stay. An unknown or retired conversation is a *StatusError with Code 404.
func (c *Client) RetireConversation(ctx context.Context, conversation int64) error {
	return c.do(ctx, http.MethodPost, "/conversations/"+itoa(conversation)+"/retire", nil, nil)
}

// Model returns the model a conversation runs on.
func (c *Client) Model(ctx context.Context, conversation int64) (ModelRef, error) {
	var out ModelRef
	err := c.do(ctx, http.MethodGet, "/conversations/"+itoa(conversation)+"/model", nil, &out)
	return out, err
}

// SetModel switches a conversation to another configured model, from its next model request
// on. A model that isn't configured is a *StatusError with Code 400.
func (c *Client) SetModel(ctx context.Context, conversation int64, model ModelRef) error {
	return c.do(ctx, http.MethodPut, "/conversations/"+itoa(conversation)+"/model", model, nil)
}

// Submit admits input to a conversation and returns the submission ID.
func (c *Client) Submit(ctx context.Context, conversation int64, in Input) (int64, error) {
	var out struct {
		SubmissionID int64 `json:"submissionId"`
	}
	err := c.do(ctx, http.MethodPost, "/conversations/"+itoa(conversation)+"/submit", in, &out)
	return out.SubmissionID, err
}

// Wait blocks until the submission settles or ctx ends.
func (c *Client) Wait(ctx context.Context, submission int64) (Settled, error) {
	var out Settled
	err := c.do(ctx, http.MethodPost, "/submissions/"+itoa(submission)+"/wait", nil, &out)
	return out, err
}

// Messages returns the model's current context for a conversation.
func (c *Client) Messages(ctx context.Context, conversation int64) ([]Message, error) {
	var out []Message
	err := c.do(ctx, http.MethodGet, "/conversations/"+itoa(conversation)+"/messages", nil, &out)
	return out, err
}

// Abort stops a conversation's work, as Stop does: queued input is withdrawn and the running
// work aborted. It returns once the conversation is idle.
func (c *Client) Abort(ctx context.Context, conversation int64) error {
	return c.do(ctx, http.MethodPost, "/conversations/"+itoa(conversation)+"/abort", nil, nil)
}

// Entries returns, as the sidecar sent it, the page of up to limit entries just before the
// entry before (the newest ones if before is 0), oldest first: {"entries": […], "before": id},
// where before, the entry just before the page, is absent when the page starts the
// transcript. Entries are passed on unread, as the watch's are.
func (c *Client) Entries(ctx context.Context, conversation, before int64, limit int) (json.RawMessage, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if before > 0 {
		q.Set("before", itoa(before))
	}
	var out json.RawMessage
	err := c.do(ctx, http.MethodGet, "/conversations/"+itoa(conversation)+"/entries?"+q.Encode(), nil, &out)
	return out, err
}

// Watch streams a conversation's view: first its value, then operations per commit. The
// stream ends when ctx ends, the sidecar closes it, or Close is called.
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

// maxWatchLine bounds one watch line. The first line is a whole view, but a view holds at
// most 100 entries and Pi Durable caps tool output, so a longer line means something is wrong.
const maxWatchLine = 16 << 20

// ErrWatchLineTooLong is returned by Next for a line over maxWatchLine. The stream can't
// continue; reconnect to start again from the current view.
var ErrWatchLineTooLong = errors.New("agentrt: watch line over 16 MB")

// ErrWatchMalformed is returned by Next for a line that isn't a valid event. The stream is
// closed, since a view can't be kept correct past a lost update: reconnect to start again.
var ErrWatchMalformed = errors.New("agentrt: malformed watch line")

// Watch is an open watch stream.
type Watch struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
}

// Next returns the next event, or io.EOF once the stream has ended.
func (w *Watch) Next() (Event, error) {
	if !w.scanner.Scan() {
		// Every way out of here ends the stream, so release the connection now.
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
	ev.Raw = bytes.Clone(w.scanner.Bytes()) // the scanner reuses its buffer
	return ev, nil
}

// Close stops the stream.
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
