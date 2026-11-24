package agenttools

import (
	"context"
	"encoding/json"
)

type memberReply struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	// Role is the person's place in the space asked about ("owner", "editor", "viewer"), or in
	// the workspace ("admin", "member") when no space was.
	Role string `json:"role"`
}

// listMembers lists who is in the call's workspace, or who has access to one space of it: the
// IDs that create_card and update_card take as an assignee. Only what the user can read is
// listed: the workspace's members, whom every member sees, or a space the user may read.
func (s *Service) listMembers(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID *int64 `json:"space_id"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	out := []memberReply{}
	if a.SpaceID == nil {
		members, err := s.o.Access.WorkspaceMembers(ctx, c.User, c.Workspace)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			out = append(out, memberReply{ID: m.ID, Name: m.Name, Email: m.Email, Role: string(m.Role)})
		}
	} else {
		sp, err := s.space(ctx, c, *a.SpaceID)
		if err != nil {
			return nil, err
		}
		// Asked again, with the same answer for a space of another workspace: space has checked it.
		_, members, err := s.o.Access.ReadableSpaceMembers(ctx, c.User, sp.ID)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			out = append(out, memberReply{ID: m.ID, Name: m.Name, Email: m.Email, Role: string(m.Role)})
		}
	}
	n, res, err := fitting(len(out), func(n int) any { return map[string]any{"members": out[:n]} })
	if err == nil && n < len(out) {
		res = map[string]any{"members": out[:n], "truncated": true}
	}
	return res, err
}
