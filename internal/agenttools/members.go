package agenttools

import (
	"bytes"
	"context"
	"encoding/json"

	"taskto.ai/taskto/internal/auth"
)

const membersPage = 500

type memberReply struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

func positiveID(raw json.RawMessage, name string) (int64, error) {
	var id int64
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&id); err != nil || dec.More() || id < 1 {
		return 0, invalidf("%s must be a whole number of 1 or more", name)
	}
	return id, nil
}

func (s *Service) listMembers(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID json.RawMessage `json:"space_id"`
		Cursor  json.RawMessage `json:"cursor"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	page := auth.RosterPage{ByID: true, Limit: membersPage + 1}
	var err error
	if a.Cursor != nil {
		if page.AfterID, err = positiveID(a.Cursor, "cursor"); err != nil {
			return nil, err
		}
	}
	out := []memberReply{}
	if a.SpaceID == nil {
		members, err := s.o.Access.WorkspaceMembers(ctx, c.User, c.Workspace, page)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			out = append(out, memberReply{ID: m.ID, Name: m.Name, Email: m.Email, Role: string(m.Role)})
		}
	} else {
		spaceID, err := positiveID(a.SpaceID, "space_id")
		if err != nil {
			return nil, err
		}
		_, members, err := s.o.Access.ReadableSpaceMembers(ctx, c.User, c.Workspace, spaceID, page)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			out = append(out, memberReply{ID: m.ID, Name: m.Name, Email: m.Email, Role: string(m.Role)})
		}
	}
	more := len(out) > membersPage
	if more {
		out = out[:membersPage]
	}
	_, res, err := fitting(len(out), func(n int) any {
		reply := map[string]any{"members": out[:n]}
		if (n < len(out) || more) && n > 0 {
			reply["next_cursor"] = out[n-1].ID
		}
		return reply
	})
	return res, err
}
