package dictation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
)

type apiEngine struct {
	client  *http.Client
	baseURL string
	model   string
	key     string
}

func (e apiEngine) String() string { return "api " + e.baseURL }

const maxReply = 4 << 20

type apiError struct{ status int }

func (e apiError) Error() string {
	return fmt.Sprintf("the speech-to-text API answered %d %s", e.status, http.StatusText(e.status))
}

func (e apiEngine) Transcribe(ctx context.Context, audio Audio, opts Options) (string, error) {
	body, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		pw.CloseWithError(writeForm(mw, audio, e.model, opts.Language))
	}()
	defer body.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/audio/transcriptions", body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	if e.key != "" {
		req.Header.Set("Authorization", "Bearer "+e.key)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxReply))
		return "", apiError{resp.StatusCode}
	}
	var reply struct {
		Text *string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReply)).Decode(&reply); err != nil || reply.Text == nil {
		return "", fmt.Errorf("the speech-to-text API's answer has no text")
	}
	return strings.TrimSpace(*reply.Text), nil
}

func writeForm(mw *multipart.Writer, audio Audio, model, language string) error {
	fw, err := mw.CreateFormFile("file", "dictation.wav")
	if err != nil {
		return err
	}
	if _, err := fw.Write(audio.WAV()); err != nil {
		return err
	}
	fields := [][2]string{{"model", model}, {"response_format", "json"}}
	if language != "" {
		fields = append(fields, [2]string{"language", language})
	}
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return err
		}
	}
	return mw.Close()
}
