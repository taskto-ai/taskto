package conversations

import "regexp"

var leadingAgent = regexp.MustCompile(`^@\[[^\]\[\\\n]{1,100}\]\(agent:([A-Za-z0-9._-]{1,256})/([A-Za-z0-9._:-]{1,256})\)(?:\s|$)`)

func agentMention(content string) (modelRef, bool) {
	m := leadingAgent.FindStringSubmatch(content)
	if m == nil {
		return modelRef{}, false
	}
	return modelRef{Provider: m[1], ModelID: m[2]}, true
}

type SendReply struct {
	SubmissionID int64     `json:"submission_id"`
	Model        *modelRef `json:"model,omitempty"`
}
