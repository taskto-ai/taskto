package agenttools

import (
	"bytes"
	"context"
	"encoding/json"

	"taskto.ai/taskto/internal/auth"
)

// membersPage is the most people one list_members reply holds, whatever their size: a
// workspace can have many more.
const membersPage = 500

type memberReply struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	// Role is the person's place in the space asked about ("owner", "editor", "viewer"), or in
	// the workspace ("admin", "member") when no space was.
	Role string `json:"role"`
}

// positiveID reads an argument that, when it is there, is a whole number of 1 or more. A null
// is refused like any other value that isn't one: it is never the same as leaving the argument
// out, which is what widens a call.
func positiveID(raw json.RawMessage, name string) (int64, error) {
	var id int64
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&id); err != nil || dec.More() || id < 1 {
		return 0, invalidf("%s must be a whole number of 1 or more", name)
	}
	return id, nil
}

// listMembers lists who is in the call's workspace, or who has access to one space of it: the
// IDs that create_card and update_card take as an assignee. Only what the user can read is
// listed: the workspace's members, whom every member sees, or a space the user may read. They
// come by user ID, a page at a time: next_cursor, when there are more, is the cursor of the
// next page.
func (s *Service) listMembers(ctx context.Context, c *call, args json.RawMessage) (any, error) {
	var a struct {
		// Both are raw so that a null is told from an omitted argument.
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
		// The space is checked, as being of this workspace too, with the list.
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
	// The size measured is that of the reply sent, next_cursor included: a reply that is cut
	// short, or that has more after it, says where the next page starts.
	_, res, err := fitting(len(out), func(n int) any {
		reply := map[string]any{"members": out[:n]}
		if (n < len(out) || more) && n > 0 {
			reply["next_cursor"] = out[n-1].ID
		}
		return reply
	})
	return res, err
}
