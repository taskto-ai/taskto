package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/auth"
)

type Scope string

const (
	ScopeUser      Scope = "user"
	ScopeSpace     Scope = "space"
	ScopeWorkspace Scope = "workspace"
)

const (
	MaxTitle       = 100
	MaxDescription = 300
	MaxBody        = 32 * 1024
	MaxPerOwner    = 200
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,39}$`)

type Skill struct {
	ID          int64  `json:"id"`
	Scope       Scope  `json:"scope"`
	SpaceID     *int64 `json:"space_id,omitempty"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Enabled     bool   `json:"enabled"`
	Origin      string `json:"origin"`
	UpdatedAt   string `json:"updated_at"`
}

type SkillList struct {
	Skills []Skill `json:"skills" apitypes:"nonnull"`
}

type Owner struct {
	Scope           Scope
	WorkspaceID     int64
	UserID          int64
	SpaceID         int64
	SpaceGeneration int64
}

type Input struct {
	Name        string
	Title       string
	Description string
	Body        string
	Enabled     *bool
	Origin      string
}

type Patch struct {
	Name        *string
	Title       *string
	Description *string
	Body        *string
	Enabled     *bool
}

func (p Patch) empty() bool {
	return p.Name == nil && p.Title == nil && p.Description == nil && p.Body == nil && p.Enabled == nil
}

var (
	ErrNotFound    = apierr.New(http.StatusNotFound, apierr.CodeSkillNotFound, "skill not found")
	errNoFields    = apierr.New(http.StatusBadRequest, apierr.CodeBadRequest, "nothing to change")
	errNoWorkspace = apierr.New(http.StatusNotFound, apierr.CodeWorkspaceNotFound, "workspace not found")
	errAdminOnly   = apierr.New(http.StatusForbidden, apierr.CodeAdminOnly, "only a workspace admin can do this")
)

type Store struct {
	db     *sql.DB
	spaces *access.Checker
}

func New(db *sql.DB, spaces *access.Checker) *Store {
	return &Store{db: db, spaces: spaces}
}

const columns = `id, scope, space_id, name, title, description, body, enabled, origin, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scan(row scanner) (Skill, error) {
	var s Skill
	if err := row.Scan(&s.ID, &s.Scope, &s.SpaceID, &s.Name, &s.Title, &s.Description, &s.Body, &s.Enabled, &s.Origin, &s.UpdatedAt); err != nil {
		return Skill{}, err
	}
	return s, nil
}

func collect(rows *sql.Rows, err error) ([]Skill, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Skill{}
	for rows.Next() {
		s, err := scan(rows)
		if err != nil {
			return nil, err
		}
		all = append(all, s)
	}
	return all, rows.Err()
}

const ownerWhere = `workspace_id = ? AND scope = ? AND user_id IS ? AND space_id IS ? AND space_generation IS ?`

func (o Owner) args() []any {
	var user, space, generation any
	switch o.Scope {
	case ScopeUser:
		user = o.UserID
	case ScopeSpace:
		space, generation = o.SpaceID, o.SpaceGeneration
	}
	return []any{o.WorkspaceID, string(o.Scope), user, space, generation}
}

func (s *Store) List(ctx context.Context, o Owner, actor int64) (list []Skill, err error) {
	err = s.spaces.Snapshot(ctx, func(_ *access.Checker, tx *sql.Tx) error {
		manages, err := s.authorize(ctx, tx, o, actor)
		if err != nil {
			return err
		}
		query := `SELECT ` + columns + ` FROM skills WHERE ` + ownerWhere + ` AND enabled = 1 ORDER BY name, id`
		if manages {
			query = `SELECT ` + columns + ` FROM skills WHERE ` + ownerWhere + ` ORDER BY name, id`
		}
		list, err = collect(tx.QueryContext(ctx, query, o.args()...))
		return err
	})
	return list, err
}

func (s *Store) authorize(ctx context.Context, tx *sql.Tx, o Owner, actor int64) (manages bool, err error) {
	role, member, err := auth.MemberRole(ctx, tx, o.WorkspaceID, actor)
	switch {
	case err != nil:
		return false, err
	case !member:
		return false, errNoWorkspace
	}
	switch o.Scope {
	case ScopeUser:
		if o.UserID != actor {
			return false, ErrNotFound
		}
		return true, nil
	case ScopeWorkspace:
		return role == auth.RoleAdmin, nil
	case ScopeSpace:
		sp, err := s.spaces.CheckTx(ctx, tx, actor, o.SpaceID)
		switch {
		case errors.Is(err, access.ErrNotFound):
			return false, access.ErrNotFound
		case err != nil:
			return false, err
		case sp.WorkspaceID != o.WorkspaceID || sp.Generation != o.SpaceGeneration || sp.Kind != access.Shared:
			return false, access.ErrNotFound
		}
		return sp.Access.Allows(access.Write), nil
	}
	return false, ErrNotFound
}

func validName(name string) *apierr.Error {
	if !namePattern.MatchString(name) {
		return apierr.New(http.StatusBadRequest, apierr.CodeSkillNameInvalid,
			"a skill's name starts with a lowercase letter and has 2 to 40 lowercase letters, digits and hyphens")
	}
	return nil
}

func textLength(value string, max int, code apierr.Code, what string) *apierr.Error {
	n := utf8.RuneCountInString(value)
	if n < 1 || n > max || !utf8.ValidString(value) || strings.ContainsFunc(value, breaksLine) {
		return apierr.New(http.StatusBadRequest, code, fmt.Sprintf("a skill's %s is 1 to %d characters on one line", what, max), apierr.Params{"max": max})
	}
	return nil
}

func breaksLine(r rune) bool {
	return unicode.IsControl(r) && r != '\t' || unicode.In(r, unicode.Zl, unicode.Zp)
}

func validBody(body string) *apierr.Error {
	if len(body) > MaxBody || strings.TrimSpace(body) == "" || !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
		return apierr.New(http.StatusBadRequest, apierr.CodeSkillBodyLength,
			fmt.Sprintf("a skill's instructions are text of 1 byte to %d KiB", MaxBody/1024), apierr.Params{"max_kib": MaxBody / 1024})
	}
	return nil
}

func (in Input) normalized() (Input, *apierr.Error) {
	in.Name = strings.Trim(in.Name, " \t")
	in.Title = strings.Trim(in.Title, " \t")
	in.Description = strings.Trim(in.Description, " \t")
	for _, err := range []*apierr.Error{
		validName(in.Name),
		textLength(in.Title, MaxTitle, apierr.CodeSkillTitleLength, "title"),
		textLength(in.Description, MaxDescription, apierr.CodeSkillDescriptionLength, "description"),
		validBody(in.Body),
	} {
		if err != nil {
			return Input{}, err
		}
	}
	return in, nil
}

func (p Patch) normalized() (Patch, *apierr.Error) {
	if p.empty() {
		return Patch{}, errNoFields
	}
	trim := func(v **string) {
		if *v != nil {
			t := strings.Trim(**v, " \t")
			*v = &t
		}
	}
	trim(&p.Name)
	trim(&p.Title)
	trim(&p.Description)
	if p.Name != nil {
		if err := validName(*p.Name); err != nil {
			return Patch{}, err
		}
	}
	if p.Title != nil {
		if err := textLength(*p.Title, MaxTitle, apierr.CodeSkillTitleLength, "title"); err != nil {
			return Patch{}, err
		}
	}
	if p.Description != nil {
		if err := textLength(*p.Description, MaxDescription, apierr.CodeSkillDescriptionLength, "description"); err != nil {
			return Patch{}, err
		}
	}
	if p.Body != nil {
		if err := validBody(*p.Body); err != nil {
			return Patch{}, err
		}
	}
	return p, nil
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func nameTaken() *apierr.Error {
	return apierr.New(http.StatusConflict, apierr.CodeSkillNameTaken, "a skill of this name exists already")
}

func (s *Store) write(ctx context.Context, o Owner, actor int64, fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rbErr := tx.Rollback(); !errors.Is(rbErr, sql.ErrTxDone) {
				err = errors.Join(err, rbErr)
			}
		}
	}()
	manages, err := s.authorize(ctx, tx, o, actor)
	switch {
	case err != nil:
		return err
	case !manages && o.Scope == ScopeWorkspace:
		return errAdminOnly
	case !manages:
		return access.ErrNotFound
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (o Owner) audited() bool { return o.Scope != ScopeUser }

func (o Owner) details(name string, fields []string) audit.Details {
	d := audit.Details{"scope": string(o.Scope), "name": name}
	if o.Scope == ScopeSpace {
		d["space_id"] = o.SpaceID
	}
	if fields != nil {
		d["fields"] = fields
	}
	return d
}

func (s *Store) Create(ctx context.Context, o Owner, actor int64, in Input) (Skill, error) {
	in, verr := in.normalized()
	if verr != nil {
		return Skill{}, verr
	}
	enabled := in.Enabled == nil || *in.Enabled
	var created Skill
	err := s.write(ctx, o, actor, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM skills WHERE `+ownerWhere, o.args()...).Scan(&n); err != nil {
			return err
		}
		if n >= MaxPerOwner {
			return apierr.New(http.StatusConflict, apierr.CodeTooManySkills,
				fmt.Sprintf("at most %d skills fit here: remove one first", MaxPerOwner), apierr.Params{"max": MaxPerOwner})
		}
		var userID, spaceID, generation any
		switch o.Scope {
		case ScopeUser:
			userID = o.UserID
		case ScopeSpace:
			spaceID, generation = o.SpaceID, o.SpaceGeneration
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO skills (workspace_id, scope, user_id, space_id, space_generation, name, title, description, body, enabled, origin, created_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			o.WorkspaceID, o.Scope, userID, spaceID, generation, in.Name, in.Title, in.Description, in.Body, enabled, in.Origin, actor)
		if isUniqueViolation(err) {
			return nameTaken()
		}
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if created, err = scan(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM skills WHERE id = ?`, id)); err != nil {
			return err
		}
		if !o.audited() {
			return nil
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: o.WorkspaceID, Actor: actor, Action: audit.SkillCreated,
			TargetType: audit.TargetSkill, Target: id, Details: o.details(in.Name, nil)})
	})
	return created, err
}

func (s *Store) Update(ctx context.Context, o Owner, actor, id int64, p Patch) (Skill, error) {
	p, verr := p.normalized()
	if verr != nil {
		return Skill{}, verr
	}
	var updated Skill
	err := s.write(ctx, o, actor, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE skills SET
				name = coalesce(?, name), title = coalesce(?, title), description = coalesce(?, description),
				body = coalesce(?, body), enabled = coalesce(?, enabled),
				updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE id = ? AND `+ownerWhere, append([]any{p.Name, p.Title, p.Description, p.Body, p.Enabled, id}, o.args()...)...)
		if isUniqueViolation(err) {
			return nameTaken()
		}
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrNotFound
		}
		if updated, err = scan(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM skills WHERE id = ?`, id)); err != nil {
			return err
		}
		if !o.audited() {
			return nil
		}
		fields := audit.Changed(map[string]bool{
			"name": p.Name != nil, "title": p.Title != nil, "description": p.Description != nil,
			"body": p.Body != nil, "enabled": p.Enabled != nil,
		})
		return audit.Record(ctx, tx, audit.Event{Workspace: o.WorkspaceID, Actor: actor, Action: audit.SkillUpdated,
			TargetType: audit.TargetSkill, Target: id, Details: o.details(updated.Name, fields)})
	})
	return updated, err
}

func (s *Store) Delete(ctx context.Context, o Owner, actor, id int64) error {
	return s.write(ctx, o, actor, func(tx *sql.Tx) error {
		var name, created string
		err := tx.QueryRowContext(ctx, `SELECT name, created_at FROM skills WHERE id = ? AND `+ownerWhere, append([]any{id}, o.args()...)...).Scan(&name, &created)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM skills WHERE id = ?`, id); err != nil {
			return err
		}
		if !o.audited() {
			return nil
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: o.WorkspaceID, Actor: actor, Action: audit.SkillRemoved,
			TargetType: audit.TargetSkill, Target: id, TargetCreated: created, Details: o.details(name, nil)})
	})
}

func (s *Store) ForUser(ctx context.Context, workspaceID, userID int64) (list []Skill, err error) {
	list = []Skill{}
	err = s.spaces.Snapshot(ctx, func(snap *access.Checker, tx *sql.Tx) error {
		if _, member, err := auth.MemberRole(ctx, tx, workspaceID, userID); err != nil || !member {
			return err
		}
		readable, err := snap.Readable(ctx, userID, workspaceID)
		if err != nil {
			return err
		}
		spaces := make([][2]int64, len(readable))
		for i, sp := range readable {
			spaces[i] = [2]int64{sp.ID, sp.Generation}
		}
		pairs, _ := json.Marshal(spaces)
		list, err = collect(tx.QueryContext(ctx, `SELECT `+columns+` FROM skills
			WHERE workspace_id = ? AND enabled = 1 AND (scope = 'workspace' OR (scope = 'user' AND user_id = ?)
				OR (scope = 'space' AND (space_id, space_generation) IN (SELECT j.value ->> 0, j.value ->> 1 FROM json_each(?) j)))
			ORDER BY CASE scope WHEN 'workspace' THEN 0 WHEN 'space' THEN 1 ELSE 2 END, space_id, name, id`,
			workspaceID, userID, string(pairs)))
		return err
	})
	return list, err
}
