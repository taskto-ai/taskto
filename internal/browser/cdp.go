package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// cdp is a Chrome DevTools Protocol client on Chromium's --remote-debugging-pipe: JSON
// messages, each ended by a NUL byte, commands written to the browser's fd 3 and replies and
// events read from its fd 4. Pages are reached through flat sessions (Target.attachToTarget
// with flatten), whose ID goes in each command. Events are read and dropped: the actions
// wait by asking, not by listening.
type cdp struct {
	w io.WriteCloser

	wmu sync.Mutex // one message written at a time

	mu      sync.Mutex
	next    int64
	pending map[int64]chan cdpReply
	err     error // set when the pipe broke; no call succeeds after
	done    chan struct{}
}

type cdpReply struct {
	Result json.RawMessage `json:"result"`
	Error  *cdpError       `json:"error"`
}

// cdpError is the browser's refusal of a command.
type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string { return fmt.Sprintf("devtools: %s (%d)", e.Message, e.Code) }

// maxMessage bounds one message from the browser.
const maxMessage = 32 << 20

// errPipeClosed is a call's error once the browser is gone.
var errPipeClosed = errors.New("the browser closed its DevTools pipe")

// newCDP starts reading replies from r. Closing w (Close) ends the browser, which exits when
// its pipe closes.
func newCDP(w io.WriteCloser, r io.Reader) *cdp {
	c := &cdp{w: w, pending: map[int64]chan cdpReply{}, done: make(chan struct{})}
	go c.read(r)
	return c
}

func (c *cdp) read(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	var err error
	for {
		var msg []byte
		msg, err = readMessage(br)
		if err != nil {
			break
		}
		var m struct {
			ID *int64 `json:"id"`
			cdpReply
		}
		if json.Unmarshal(msg, &m) != nil || m.ID == nil {
			continue // an event, or noise
		}
		c.mu.Lock()
		ch := c.pending[*m.ID]
		delete(c.pending, *m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m.cdpReply
		}
	}
	if errors.Is(err, io.EOF) {
		err = errPipeClosed
	}
	c.mu.Lock()
	c.err = err
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.done)
}

// readMessage reads one NUL-ended message, refusing one over maxMessage.
func readMessage(br *bufio.Reader) ([]byte, error) {
	var msg []byte
	for {
		chunk, err := br.ReadSlice(0)
		msg = append(msg, chunk...)
		if len(msg) > maxMessage {
			return nil, errors.New("devtools: a message over 32 MiB")
		}
		switch {
		case err == nil:
			return msg[:len(msg)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}

// Call sends method with params to the browser (session "" ) or a page's session, and decodes
// the result into out (if not nil).
func (c *cdp) Call(ctx context.Context, session, method string, params, out any) error {
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.next++
	id := c.next
	ch := make(chan cdpReply, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if session != "" {
		msg["sessionId"] = session
	}
	b, err := json.Marshal(msg)
	if err != nil {
		c.forget(id)
		return err
	}
	c.wmu.Lock()
	_, err = c.w.Write(append(b, 0))
	c.wmu.Unlock()
	if err != nil {
		c.forget(id)
		return fmt.Errorf("devtools: writing to the browser: %w", err)
	}
	select {
	case rep, ok := <-ch:
		if !ok {
			return c.closedErr()
		}
		if rep.Error != nil {
			return rep.Error
		}
		if out != nil && len(rep.Result) > 0 {
			return json.Unmarshal(rep.Result, out)
		}
		return nil
	case <-ctx.Done():
		c.forget(id)
		return ctx.Err()
	}
}

func (c *cdp) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *cdp) closedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return errPipeClosed
}

// Done is closed when the browser's side of the pipe is gone.
func (c *cdp) Done() <-chan struct{} { return c.done }

// Close closes the command pipe: the browser exits when it sees it closed.
func (c *cdp) Close() error { return c.w.Close() }
