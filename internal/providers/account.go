package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"taskto.ai/taskto/internal/apierr"
)

type Scope string

const (
	ScopeWorkspace Scope = "workspace"
	ScopeUser      Scope = "user"
	ScopeInstance  Scope = "instance"
	ScopeCLI       Scope = "cli"
)

const CLIProviderPrefix = "cli-"

const KindOpenAICompatible = "openai-compatible"

const (
	maxNameLen        = 40
	maxDisplayNameLen = 100
	maxURLLen         = 2048
	minKeyLen         = 8
	maxKeyLen         = 4096
	maxModels         = 100
	maxModelText      = 256
	maxSafeInt        = 1<<53 - 1
	maxAccounts       = 50
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int64  `json:"context_window,omitempty"`
	MaxTokens     int64  `json:"max_tokens,omitempty"`
	Reasoning     bool   `json:"reasoning"`
}

type Account struct {
	ID          int64           `json:"id"`
	Scope       Scope           `json:"scope"`
	Provider    string          `json:"provider"`
	Name        string          `json:"name"`
	DisplayName string          `json:"display_name"`
	Kind        string          `json:"kind"`
	BaseURL     string          `json:"base_url"`
	APIKey      string          `json:"api_key"`
	Compat      json.RawMessage `json:"compat,omitempty"`
	Models      []Model         `json:"models"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
}

const (
	keySet    = "set"
	keyNotSet = "not set"
)

type draft struct {
	name        string
	displayName string
	kind        string
	baseURL     string
	apiKey      *string
	keepKey     bool
	compat      json.RawMessage
	models      []Model
}

func invalid(code apierr.Code, params apierr.Params, format string, args ...any) *apierr.Error {
	return apierr.New(http.StatusBadRequest, code, fmt.Sprintf(format, args...), params)
}

func (d *draft) validate(hasKey bool) error {
	if len(d.name) > maxNameLen || !nameRe.MatchString(d.name) {
		return invalid(apierr.CodeProviderNameInvalid, apierr.Params{"max": maxNameLen},
			"name must be 1 to %d characters of a-z, 0-9, '.', '_' or '-', starting with a letter or digit", maxNameLen)
	}
	d.displayName = strings.TrimSpace(d.displayName)
	if d.displayName == "" {
		d.displayName = d.name
	}
	if n := utf8.RuneCountInString(d.displayName); n > maxDisplayNameLen || !printable(d.displayName) {
		return invalid(apierr.CodeProviderDisplayNameInvalid, apierr.Params{"max": maxDisplayNameLen},
			"display_name must be up to %d printable characters", maxDisplayNameLen)
	}
	if d.kind != KindOpenAICompatible {
		return invalid(apierr.CodeBadRequest, nil, "kind must be %q", KindOpenAICompatible)
	}
	var err error
	if d.baseURL, d.apiKey, err = CheckEndpoint(d.baseURL, d.apiKey, hasKey); err != nil {
		return err
	}
	if d.compat, err = checkCompat(d.compat); err != nil {
		return err
	}
	return checkModels(d.models)
}

func CheckEndpoint(baseURL string, apiKey *string, hasKey bool) (string, *string, error) {
	u, err := checkBaseURL(baseURL)
	if err != nil {
		return "", nil, err
	}
	if apiKey != nil {
		key := strings.TrimSpace(*apiKey)
		if len(key) < minKeyLen || len(key) > maxKeyLen || !keyChars(key) {
			return "", nil, invalid(apierr.CodeAPIKeyInvalid, apierr.Params{"min": minKeyLen, "max": maxKeyLen},
				"api_key must be %d to %d printable ASCII characters without spaces", minKeyLen, maxKeyLen)
		}
		apiKey = &key
	}
	if hasKey && !keySafeOver(u) {
		return "", nil, invalid(apierr.CodeBaseURLInsecure, nil, "base_url must use https to send an API key to another host")
	}
	return u.String(), apiKey, nil
}

func checkBaseURL(raw string) (*url.URL, error) {
	bad := invalid(apierr.CodeBaseURLInvalid, apierr.Params{"max": maxURLLen},
		"base_url must be an http or https URL of up to %d characters, without a query or fragment", maxURLLen)
	if raw == "" || len(raw) > maxURLLen {
		return nil, bad
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= ' ' || raw[i] >= 0x7f {
			return nil, bad
		}
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, bad
	}
	if u.User != nil {
		return nil, invalid(apierr.CodeBaseURLCredentials, nil, "base_url must not contain credentials; use api_key")
	}
	if !checkHost(u) {
		return nil, bad
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

var hostRe = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*\.?$`)

func checkHost(u *url.URL) bool {
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n > 65535 {
			return false
		}
	}
	host := u.Hostname()
	if strings.HasPrefix(u.Host, "[") {
		ip := net.ParseIP(host)
		return ip != nil && ip.To4() == nil && !strings.Contains(host, "%")
	}
	if !hostRe.MatchString(host) {
		return false
	}
	labels := strings.Split(strings.TrimSuffix(host, "."), ".")
	last := strings.ToLower(labels[len(labels)-1])
	if strings.Trim(last, "0123456789") == "" || strings.HasPrefix(last, "0x") {
		ip := net.ParseIP(strings.TrimSuffix(host, "."))
		return ip != nil && ip.To4() != nil && len(labels) == 4
	}
	return true
}

var loopbackV4 = regexp.MustCompile(`^127\.\d+\.\d+\.\d+$`)

func keySafeOver(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	host := u.Hostname()
	return host == "localhost" || host == "::1" || loopbackV4.MatchString(host)
}

func keyChars(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

func printable(s string) bool {
	for _, r := range s {
		if r < ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

func jsLen(s string) int {
	n := 0
	for _, r := range s {
		n += max(utf16.RuneLen(r), 1)
	}
	return n
}

func checkModels(models []Model) error {
	if len(models) == 0 || len(models) > maxModels {
		return invalid(apierr.CodeProviderModelsCount, apierr.Params{"max": maxModels}, "models must list 1 to %d models", maxModels)
	}
	seen := map[string]bool{}
	for i := range models {
		m := &models[i]
		at := "models[" + strconv.Itoa(i) + "]"
		if m.ID == "" || jsLen(m.ID) > maxModelText || !printable(m.ID) {
			return invalid(apierr.CodeProviderModelInvalid, apierr.Params{"max": maxModelText},
				"%s.id must be 1 to %d printable characters", at, maxModelText)
		}
		if seen[m.ID] {
			return invalid(apierr.CodeBadRequest, nil, "%s.id is a duplicate", at)
		}
		seen[m.ID] = true
		m.Name = strings.TrimSpace(m.Name)
		if m.Name == "" {
			m.Name = m.ID
		}
		if jsLen(m.Name) > maxModelText || !printable(m.Name) {
			return invalid(apierr.CodeBadRequest, nil, "%s.name must be up to %d printable characters", at, maxModelText)
		}
		if m.ContextWindow < 0 || m.ContextWindow > maxSafeInt {
			return invalid(apierr.CodeBadRequest, nil, "%s.context_window must be a positive integer", at)
		}
		if m.MaxTokens < 0 || m.MaxTokens > maxSafeInt {
			return invalid(apierr.CodeBadRequest, nil, "%s.max_tokens must be a positive integer", at)
		}
	}
	return nil
}

var compatSettings = map[string][]string{
	"supportsStore":                               nil,
	"supportsDeveloperRole":                       nil,
	"supportsReasoningEffort":                     nil,
	"supportsUsageInStreaming":                    nil,
	"supportsFinishReason":                        nil,
	"maxTokensField":                              {"max_completion_tokens", "max_tokens"},
	"requiresToolResultName":                      nil,
	"requiresAssistantAfterToolResult":            nil,
	"requiresNonNullAssistantContent":             nil,
	"requiresToolsWithToolHistory":                nil,
	"requiresThinkingAsText":                      nil,
	"requiresReasoningContentOnAssistantMessages": nil,
	"thinkingFormat": {"openai", "openrouter", "deepseek", "together", "baseten", "zai", "qwen",
		"chat-template", "qwen-chat-template", "string-thinking", "ant-ling"},
	"zaiToolStream":                  nil,
	"thinkingTokenBudgetField":       {"thinking_token_budget", "thinking_budget", "thinking_budget_tokens"},
	"supportsThinkingTokenBudget":    nil,
	"supportsOpenAIGrammarTools":     nil,
	"supportsMidConvoSystemMessages": nil,
	"supportsMidConvoToolAdditions":  nil,
	"supportsStrictMode":             nil,
	"cacheControlFormat":             {"anthropic"},
	"sendSessionAffinityHeaders":     nil,
	"sessionAffinityFormat":          {"openai", "openai-nosession", "openrouter"},
	"promptCacheKeyMode":             {"auto", "enabled", "disabled"},
	"supportsLongCacheRetention":     nil,
	"vllmPriority":                   {"number"},
}

func checkCompat(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil || settings == nil {
		return nil, invalid(apierr.CodeBadRequest, nil, "compat must be an object")
	}
	if len(settings) == 0 {
		return nil, nil
	}
	for name, value := range settings {
		allowed, known := compatSettings[name]
		if !known {
			return nil, invalid(apierr.CodeBadRequest, nil, "compat has an unknown or unsupported setting %q", truncate(name, 64))
		}
		ok := false
		switch {
		case allowed == nil:
			var b bool
			ok = json.Unmarshal(value, &b) == nil && string(value) != "null"
		case len(allowed) == 1 && allowed[0] == "number":
			var f float64
			ok = json.Unmarshal(value, &f) == nil && string(value) != "null"
		default:
			var s string
			ok = json.Unmarshal(value, &s) == nil && contains(allowed, s)
		}
		if !ok {
			return nil, invalid(apierr.CodeBadRequest, nil, "compat.%s has a value of the wrong type or out of range", name)
		}
	}
	out, err := json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

var providerRe = regexp.MustCompile(`^(ws|u)([0-9]+)-`)

func providerID(scope Scope, owner int64, name string) string {
	if scope == ScopeWorkspace {
		return "ws" + strconv.FormatInt(owner, 10) + "-" + name
	}
	return "u" + strconv.FormatInt(owner, 10) + "-" + name
}

func Usable(provider string, user, workspace int64) bool {
	m := providerRe.FindStringSubmatch(provider)
	if m == nil {
		return true
	}
	owner, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return false
	}
	if m[1] == "ws" {
		return owner == workspace
	}
	return owner == user
}

func reserved(provider string) bool { return providerRe.MatchString(provider) }
