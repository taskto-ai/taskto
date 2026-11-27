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

const maxRecipients = 50

type envelope struct {
	from string
	to   []string
	data []byte
}

func envelopeAddress(a *netmail.Address) string {
	bare := (&netmail.Address{Address: a.Address}).String()
	return strings.TrimSuffix(strings.TrimPrefix(bare, "<"), ">")
}

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
	env := &envelope{from: envelopeAddress(from)}
	var to []string
	for _, raw := range m.To {
		a, err := netmail.ParseAddress(raw)
		if err != nil {
			return nil, errors.New("mail: a recipient isn't an email address")
		}
		env.to = append(env.to, envelopeAddress(a))
		to = append(to, a.String())
	}

	var buf bytes.Buffer
	header := func(name, value string) { buf.WriteString(name + ": " + value + "\r\n") }
	header("From", from.String())
	header("To", strings.Join(to, ",\r\n "))
	header("Subject", mime.QEncoding.Encode("utf-8", strings.TrimSpace(m.Subject)))
	header("Date", now.Format(time.RFC1123Z))
	id, err := messageID(from.Address)
	if err != nil {
		return nil, err
	}
	header("Message-ID", id)
	header("MIME-Version", "1.0")
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

func writeQP(w interface{ Write([]byte) (int, error) }, text string) error {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	text = strings.ReplaceAll(text, "\n", "\r\n")
	qp := quotedprintable.NewWriter(w)
	if _, err := qp.Write([]byte(text)); err != nil {
		return err
	}
	return qp.Close()
}

func messageID(from string) (string, error) {
	domain := "localhost"
	if i := strings.LastIndex(from, "@"); i >= 0 && i < len(from)-1 {
		domain = from[i+1:]
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "<" + hex.EncodeToString(b[:]) + "@" + domain + ">", nil
}
