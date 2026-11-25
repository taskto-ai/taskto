package export

// An admin's export of a workspace (docs/api.md#export-a-workspace): the shared spaces the admin
// is a member of, with their documents, boards and apps, and the list of the workspace's members.
//
// It never widens what the admin can read. Workspace admins don't read the shared spaces they
// aren't members of (internal/access), so those are not in it: to export one, the admin joins it
// first, which every member of it sees. Nobody's private space is in it, nor any conversation.
// The space's content is read through the same packages, as the admin, which check their access
// again on each read, and the documents, which the knowledge base reads without asking whose
// request it is, are guarded by a check of the admin's membership right before each file
// (run.check). A space the admin leaves while the export runs is left out from there on, and the
// manifest says so.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/auth"
)

// membersPage is how many members are read at a time.
const membersPage = 500

// prepareWorkspace finds the shared spaces the admin is a member of. What can fail with a proper
// answer is read now, before any byte is sent.
func (s *Service) prepareWorkspace(ctx context.Context, u auth.User, m auth.Membership) (*run, error) {
	readable, err := s.spaces.Readable(ctx, u.ID, m.WorkspaceID)
	if err != nil {
		return nil, err
	}
	ex := &run{svc: s, user: u, ws: m, at: s.now().UTC(), names: newNames()}
	for _, sp := range readable {
		// Readable is the spaces the admin may read: their own private space, and the shared
		// spaces they are a member of. Only the shared ones are exported.
		if sp.Kind == access.Shared && sp.Role != "" {
			ex.shared = append(ex.shared, sp)
		}
	}
	ex.mf = manifest{
		Format: Format, Scope: "workspace", ExportedAt: ex.at.Format(time.RFC3339),
		Workspace: manifestWS{ID: m.WorkspaceID, Name: m.WorkspaceName},
		User:      manifestUser{ID: u.ID, Name: u.Name, Email: u.Email},
		Counts:    map[string]int{"members": 0, "spaces": 0, "documents": 0, "boards": 0, "cards": 0, "comments": 0, "apps": 0, "records": 0},
		Spaces:    []manifestSpace{},
		Skipped:   []skipped{},
	}
	return ex, nil
}

// memberFile is one member of the workspace in members.json.
type memberFile struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	JoinedAt string `json:"joined_at"`
}

// members writes members.json: the workspace's members, every one of them with their name, email
// and role, which every member of the workspace sees too. They are read a page at a time and
// written as they come.
func (ex *run) members(ctx context.Context) error {
	if err := ex.checkAdmin(ctx); err != nil {
		return err
	}
	w, err := ex.entry(ex.names.claim("members.json"), false)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "{\n  \"members\": ["); err != nil {
		return err
	}
	var after int64
	n := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Each page is read in a snapshot that begins with the check that the exporter is still an
		// admin, and in the order of IDs from the first page on: the cursor is an ID, so a first
		// page by name would repeat some members and leave others out.
		var page []auth.Member
		err := ex.readAsAdmin(ctx, func(tx *sql.Tx) (err error) {
			page, err = auth.Roster(ctx, tx, ex.ws.WorkspaceID, auth.RosterPage{ByID: true, AfterID: after, Limit: membersPage})
			return err
		})
		if errors.Is(err, errNotAdmin) {
			return err
		}
		if err != nil {
			return fmt.Errorf("reading the members: %w", err)
		}
		for _, mem := range page {
			line, err := compactJSON(memberFile{ID: mem.ID, Name: mem.Name, Email: mem.Email, Role: string(mem.Role), JoinedAt: mem.JoinedAt})
			if err != nil {
				return err
			}
			sep := ",\n    "
			if n == 0 {
				sep = "\n    "
			}
			n++
			if _, err := io.WriteString(w, sep); err != nil {
				return err
			}
			if _, err := w.Write(line); err != nil {
				return err
			}
		}
		if len(page) < membersPage {
			break
		}
		after = page[len(page)-1].ID
	}
	if n > 0 {
		if _, err := io.WriteString(w, "\n  "); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "]\n}\n"); err != nil {
		return err
	}
	ex.mf.Counts["members"] = n
	return nil
}

// spaceFile is a space's space.json.
type spaceFile struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	ArchivedAt *string `json:"archived_at"`
	CreatedAt  string  `json:"created_at"`
	// Role is the admin's own place in the space.
	Role access.Role `json:"role"`
}

// spaces writes each shared space the admin was a member of when the export began, under
// spaces/<id>-<name>/, one after the other.
func (ex *run) spaces(ctx context.Context) error {
	for _, sp := range ex.shared {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ex.space1(ctx, sp); err != nil {
			return err
		}
	}
	return nil
}

func (ex *run) space1(ctx context.Context, sp access.Space) error {
	p := place{space: sp, shared: true, dir: []string{"spaces", strconv.FormatInt(sp.ID, 10) + "-" + slug(sp.Name)}}
	// The space as the admin sees it now, which is what its own file says: a space they left is
	// skipped whole.
	cur, err := ex.current(ctx, p)
	if err != nil {
		return ex.leftOut(p, err)
	}
	file := ex.names.claim(p.path("space.json")...)
	if err := ex.json(file, spaceFile{ID: cur.ID, Name: cur.Name, ArchivedAt: cur.ArchivedAt, CreatedAt: cur.CreatedAt, Role: cur.Role}); err != nil {
		return err
	}
	ex.mf.Spaces = append(ex.mf.Spaces, manifestSpace{ID: cur.ID, Name: cur.Name, Dir: strings.TrimSuffix(file, "/space.json"), ArchivedAt: cur.ArchivedAt})
	ex.mf.Counts["spaces"]++
	for _, step := range []func(context.Context, place) error{ex.documents, ex.boards, ex.apps} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := step(ctx, p); err != nil {
			return ex.leftOut(p, err)
		}
	}
	return nil
}

// leftOut ends the export of one space if the reason is that the admin may no longer read it,
// and says so in the manifest; anything else ends the export.
func (ex *run) leftOut(p place, err error) error {
	if errors.Is(err, errLostAccess) {
		ex.skip(p, "space", "access_lost", strconv.FormatInt(p.space.ID, 10))
		return nil
	}
	return err
}
