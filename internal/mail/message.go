package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"net/textproto"
	"strings"
	"time"
)

// maxRecipients bounds one message's recipients: this is a notification mailer, not a list.
const maxRecipients = 50

// envelope is a message ready to send: the sender and recipients' bare addresses, and the
// bytes of the message.
type envelope struct {
	from string
	to   []string
	data []byte
}

// build checks m and renders it as an RFC 5322 message from from. Every header value is
// built from parsed addresses and encoded words, so nothing in a subject or a name can
// start a new header.
func build(from *netmail.Address, m Message, now time.Time) (*envelope, error) {
	if len(m.To) == 0 {
		return nil, errors.New("mail: a message needs a recipient")
	}
	if len(m.To) > maxRecipients {
		return nil, fmt.Errorf("mail: a message has at most %d recipients", maxRecipients)
	}
	if strings.TrimSpace(m.Text) == "" {
		return nil, errors.New("mail: a message needs a text body")
	}
	if strings.ContainsAny(m.Subject, "\r\n") {
		return nil, errors.New("mail: the subject is one line")
	}
	env := &envelope{from: from.Address}
	var to []string
	for _, raw := range m.To {
		a, err := netmail.ParseAddress(raw)
		if err != nil {
			return nil, errors.New("mail: a recipient isn't an email address")
		}
		env.to = append(env.to, a.Address)
		to = append(to, a.String())
	}

	var buf bytes.Buffer
	header := func(name, value string) { buf.WriteString(name + ": " + value + "\r\n") }
	header("From", from.String())
	header("To", strings.Join(to, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", strings.TrimSpace(m.Subject)))
	header("Date", now.Format(time.RFC1123Z))
	id, err := messageID(from.Address)
	if err != nil {
		return nil, err
	}
	header("Message-ID", id)
	header("MIME-Version", "1.0")
	// Not a person writing: auto-replies (out of office) shouldn't answer these.
	header("Auto-Submitted", "auto-generated")
	header("X-Auto-Response-Suppress", "All")

	if m.HTML == "" {
		header("Content-Type", `text/plain; charset="utf-8"`)
		header("Content-Transfer-Encoding", "quoted-printable")
		buf.WriteString("\r\n")
		if err := writeQP(&buf, m.Text); err != nil {
			return nil, err
		}
		env.data = buf.Bytes()
		return env, nil
	}

	mw := multipart.NewWriter(&buf)
	header("Content-Type", `multipart/alternative; boundary="`+mw.Boundary()+`"`)
	buf.WriteString("\r\n")
	// The plain text first: the last part is the one a client prefers.
	for _, part := range []struct{ typ, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		w, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.typ + `; charset="utf-8"`},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		if err := writeQP(w, part.body); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	env.data = buf.Bytes()
	return env, nil
}

// writeQP writes text quoted-printable, with CRLF line ends.
func writeQP(w interface{ Write([]byte) (int, error) }, text string) error {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	text = strings.ReplaceAll(text, "\n", "\r\n")
	qp := quotedprintable.NewWriter(w)
	if _, err := qp.Write([]byte(text)); err != nil {
		return err
	}
	return qp.Close()
}

// messageID makes a unique Message-ID in the sender's domain.
func messageID(from string) (string, error) {
	domain := "localhost"
	if _, d, ok := strings.Cut(from, "@"); ok && d != "" {
		domain = d
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "<" + hex.EncodeToString(b[:]) + "@" + domain + ">", nil
}
