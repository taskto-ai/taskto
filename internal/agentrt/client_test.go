package agentrt

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestWatchMalformedLineEndsTheStream(t *testing.T) {
	body := &closeTracker{Reader: strings.NewReader("not json\n" + `{"type":"ops","ops":[]}` + "\n")}
	w := &Watch{body: body, scanner: bufio.NewScanner(body)}
	if _, err := w.Next(); !errors.Is(err, ErrWatchMalformed) {
		t.Fatalf("err = %v, want ErrWatchMalformed", err)
	}
	if !body.closed {
		t.Fatal("the stream was left open after a malformed line")
	}
}

type closeTracker struct {
	io.Reader
	closed bool
}

func (c *closeTracker) Close() error {
	c.closed = true
	return nil
}

func TestWatchLineTooLong(t *testing.T) {
	long := `{"type":"value","value":"` + strings.Repeat("x", maxWatchLine) + `"}` + "\n"
	scanner := bufio.NewScanner(strings.NewReader(long))
	scanner.Buffer(make([]byte, 64*1024), maxWatchLine)
	w := &Watch{body: io.NopCloser(strings.NewReader("")), scanner: scanner}
	if _, err := w.Next(); !errors.Is(err, ErrWatchLineTooLong) {
		t.Fatalf("err = %v, want ErrWatchLineTooLong", err)
	}
}

func TestWatchKeepsEachLineAsSent(t *testing.T) {
	lines := []string{`{"type":"value","value":{"a": 1},"later":true}`, `{"type":"ops","ops":[["x"]]}`}
	body := strings.NewReader(strings.Join(lines, "\n") + "\n")
	w := &Watch{body: io.NopCloser(body), scanner: bufio.NewScanner(body)}
	for _, want := range lines {
		ev, err := w.Next()
		if err != nil {
			t.Fatal(err)
		}
		if string(ev.Raw) != want {
			t.Fatalf("raw = %s, want %s", ev.Raw, want)
		}
	}
}
