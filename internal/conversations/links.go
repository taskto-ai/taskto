package conversations

import (
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const maxLinked = 3

var conversationLink = regexp.MustCompile(`(?:^|[\s(<\["'])((?i:https?)://[^\s/()<>\["']+)?/w/(\d+)/conversations/(\d+)\b`)

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

func (s *Service) SetPublicURL(raw string) { s.origin = originOf(raw) }

func (s *Service) linkedIn(content string, workspace, current int64) []int64 {
	var ids []int64
	for _, m := range conversationLink.FindAllStringSubmatch(content, -1) {
		if m[1] != "" && (s.origin == "" || strings.ToLower(m[1]) != s.origin) {
			continue
		}
		ws, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil || ws != workspace {
			continue
		}
		id, err := strconv.ParseInt(m[3], 10, 64)
		if err != nil || id == current || slices.Contains(ids, id) {
			continue
		}
		ids = append(ids, id)
		if len(ids) == maxLinked {
			break
		}
	}
	return ids
}
