package cliworker

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
)

// codexLimits reads the newest usage limits Codex recorded in thread's session file (its
// "rollout"), under codexHome/sessions/YYYY/MM/DD/rollout-…-<thread>.jsonl: `codex exec
// --json` doesn't print them, but every token_count event in the file carries them. It
// returns nil when there are none (an API-key login, or a gateway, reports none).
func codexLimits(codexHome, thread string) []Limit {
	if thread == "" || !sessionPattern.MatchString(thread) {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(codexHome, "sessions", "*", "*", "*", "rollout-*-"+thread+".jsonl"))
	if len(matches) == 0 {
		return nil
	}
	f, err := os.Open(matches[len(matches)-1])
	if err != nil {
		return nil
	}
	defer f.Close()
	type window struct {
		UsedPercent   *float64 `json:"used_percent"`
		WindowMinutes int      `json:"window_minutes"`
		ResetsAt      *int64   `json:"resets_at"`
	}
	var last []Limit
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var line struct {
			Type    string `json:"type"`
			Payload struct {
				Type       string `json:"type"`
				RateLimits *struct {
					Primary   *window `json:"primary"`
					Secondary *window `json:"secondary"`
				} `json:"rate_limits"`
			} `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil || line.Payload.Type != "token_count" || line.Payload.RateLimits == nil {
			continue
		}
		var limits []Limit
		for _, w := range []struct {
			name string
			w    *window
		}{{"primary", line.Payload.RateLimits.Primary}, {"secondary", line.Payload.RateLimits.Secondary}} {
			if w.w == nil || w.w.UsedPercent == nil {
				continue
			}
			limits = append(limits, Limit{
				Window: w.name, UsedPercent: w.w.UsedPercent, WindowMinutes: w.w.WindowMinutes, ResetsAt: unixTime(w.w.ResetsAt),
			})
		}
		if len(limits) > 0 {
			last = limits
		}
	}
	return last
}
