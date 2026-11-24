package apps

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
)

// Starter templates, and copies of apps.
//
// A template is a definition (the same JSON as any app's) with a name, a description and, if it
// restricts who sees the records, the roles an install creates. The three that come with the
// server are in templates/, versioned and checked by Definition.Check like any app's definition
// (a test checks them all; a template that doesn't pass doesn't start the server's list).
//
// Installing one, and duplicating an existing app (Duplicate), do one thing: make a new app in a
// space the caller may write whose DRAFT is a copy of a definition. Nothing else is copied. Not a
// record, an attachment, a record's history, an effect's operation state, a release, a home
// placement or a role assignment of the source, and never a secret: a definition holds none, and
// what it does hold (the IDs of the boards its effects aim at) is cleared where the copier
// couldn't write them. The new app is independent of its source (installing twice makes two apps),
// is a draft until someone who may publish it does, in the ordinary way, and its first role
// assignments are made here, for the person who installed it only (and, for a template that says
// so, the default for the space's other members), in the same transaction that creates it, so
// no one else holds a role in it, even for an instant, that the template didn't give them.

// TemplateVersion is the version of the format of a template file.
const TemplateVersion = 1

//go:embed templates/*.json
var templatesFS embed.FS

// TemplateRoles are the roles an install gives, with the role assignments of the new app: the
// installer's own, and the default of every other member of a shared space. A template without
// roles leaves the app unrestricted, which is what the space's own rules then decide.
type TemplateRoles struct {
	Installer []access.AppRole `json:"installer" apitypes:"nonnull"`
	Members   []access.AppRole `json:"members,omitempty"`
}

// Template is a starter app.
type Template struct {
	// TemplateVersion is the format of the file; ID names the template, Version its own revision.
	TemplateVersion int    `json:"template_version"`
	ID              string `json:"id"`
	Version         int    `json:"version"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	// RecommendedSpace is "private" or "shared": where it is meant to be used. The caller chooses.
	RecommendedSpace string         `json:"recommended_space"`
	Roles            *TemplateRoles `json:"roles,omitempty"`
	Definition       Definition     `json:"definition"`
}

// TemplateInfo is what the API tells of a template.
type TemplateInfo struct {
	ID               string `json:"id"`
	Version          int    `json:"version"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	RecommendedSpace string `json:"recommended_space"`
	// NeedsBoard is whether installing it needs a board: its effects make cards on one, which the
	// installer chooses among those they may write.
	NeedsBoard bool `json:"needs_board"`
	// Captures are the kinds of field that need the phone's own hardware: attachment (camera),
	// location, signature and barcode.
	Captures []string `json:"captures" apitypes:"nonnull"`
	// Roles are the role assignments an install makes, if any.
	Roles *TemplateRoles `json:"roles,omitempty"`
	// Widgets are the titles of the Home widgets it offers.
	Widgets []string `json:"widgets" apitypes:"nonnull"`
}

var (
	templatesOnce sync.Once
	templates     []Template
	templatesErr  error
)

var templateID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// Templates returns the bundled templates by ID, each checked as a saved app is.
func Templates() ([]Template, error) {
	templatesOnce.Do(func() { templates, templatesErr = loadTemplates() })
	return templates, templatesErr
}

func loadTemplates() ([]Template, error) {
	entries, err := templatesFS.ReadDir("templates")
	if err != nil {
		return nil, err
	}
	var all []Template
	seen := map[string]bool{}
	for _, e := range entries {
		raw, err := templatesFS.ReadFile("templates/" + e.Name())
		if err != nil {
			return nil, err
		}
		var t Template
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&t); err != nil {
			return nil, fmt.Errorf("template %s: %w", e.Name(), err)
		}
		if err := t.check(); err != nil {
			return nil, fmt.Errorf("template %s: %w", e.Name(), err)
		}
		if seen[t.ID] {
			return nil, fmt.Errorf("template %s: the id %q is used twice", e.Name(), t.ID)
		}
		seen[t.ID] = true
		all = append(all, t)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return all, nil
}

// check validates a template and replaces its definition with the checked one.
func (t *Template) check() error {
	if t.TemplateVersion != TemplateVersion {
		return fmt.Errorf("template_version is %d: this server reads %d", t.TemplateVersion, TemplateVersion)
	}
	if !templateID.MatchString(t.ID) || t.Version < 1 {
		return fmt.Errorf("an id is lowercase letters, digits and -, and a version is from 1")
	}
	if strings.TrimSpace(t.Name) == "" || strings.TrimSpace(t.Description) == "" {
		return fmt.Errorf("a name and a description are required")
	}
	if t.RecommendedSpace != "private" && t.RecommendedSpace != "shared" {
		return fmt.Errorf("recommended_space is private or shared")
	}
	if t.Roles != nil {
		if len(t.Roles.Installer) == 0 {
			return fmt.Errorf("roles: the installer needs at least one, or they couldn't manage the app")
		}
		// The installer must be able to build and publish what they install and manage who may.
		for _, need := range []access.AppRole{access.AppBuilder, access.AppPublisher} {
			if !slices.Contains(t.Roles.Installer, need) {
				return fmt.Errorf("roles: the installer needs the %s role", need)
			}
		}
	}
	def, err := t.Definition.Check()
	if err != nil {
		return err
	}
	// Records, and so a template's data, never come with it: a template is a definition.
	t.Definition = def
	return nil
}

// Info describes the template for the API.
func (t Template) info() TemplateInfo {
	info := TemplateInfo{
		ID: t.ID, Version: t.Version, Name: t.Name, Description: t.Description, RecommendedSpace: t.RecommendedSpace,
		NeedsBoard: len(t.Definition.unsetTargets()) > 0, Roles: t.Roles, Captures: []string{}, Widgets: []string{},
	}
	for _, f := range t.Definition.Collection.Fields {
		switch f.Type {
		case TypeAttachment, TypeLocation, TypeSignature, TypeBarcode:
			if !slices.Contains(info.Captures, f.Type) {
				info.Captures = append(info.Captures, f.Type)
			}
		}
	}
	if c := t.Definition.Contributions; c != nil {
		for _, w := range c.Widgets {
			info.Widgets = append(info.Widgets, w.Title)
		}
	}
	return info
}

// TemplateInfos lists the bundled templates.
func TemplateInfos() ([]TemplateInfo, error) {
	all, err := Templates()
	if err != nil {
		return nil, err
	}
	out := make([]TemplateInfo, 0, len(all))
	for _, t := range all {
		out = append(out, t.info())
	}
	return out, nil
}

// cloneDefinition copies a definition through its JSON, so a copy shares no slice or map with it.
func cloneDefinition(d Definition) (Definition, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return Definition{}, err
	}
	var out Definition
	if err := json.Unmarshal(raw, &out); err != nil {
		return Definition{}, err
	}
	return out, nil
}

// BoardRef is a board an effect acts on, chosen by the person installing or copying.
type BoardRef struct {
	SpaceID int64 `json:"space_id"`
	BoardID int64 `json:"board_id"`
}

// Install is a request to install a template into a space.
type Install struct {
	// Name is the new app's name; the template's if left out.
	Name string `json:"name"`
	// Board is the board the template's effects make cards on: required when the template needs
	// one (TemplateInfo.NeedsBoard), refused when it doesn't.
	Board *BoardRef `json:"board"`
}

// installTemplate makes a new app in sp from the template id: a draft with the template's
// definition and, if it has roles, the installer's own assignments.
func (s *Service) installTemplate(ctx context.Context, sp access.Space, a Actor, id string, in Install) (App, error) {
	if err := may(sp, access.Write); err != nil {
		return App{}, err
	}
	all, err := Templates()
	if err != nil {
		return App{}, err
	}
	var t *Template
	for i := range all {
		if all[i].ID == id {
			t = &all[i]
		}
	}
	if t == nil {
		return App{}, &statusError{http.StatusNotFound, apierr.CodeAppTemplateNotFound, "no such template", nil}
	}
	def, err := cloneDefinition(t.Definition)
	if err != nil {
		return App{}, err
	}
	needs := len(def.unsetTargets()) > 0
	switch {
	case needs && in.Board == nil:
		return App{}, invalidAs(apierr.CodeAppTemplateBoard, nil, "this template makes cards on a board: choose one you can write")
	case !needs && in.Board != nil:
		return App{}, invalidAs(apierr.CodeAppTemplateBoard, nil, "this template doesn't use a board")
	case needs:
		if err := s.chooseBoard(ctx, sp, a, *in.Board); err != nil {
			return App{}, err
		}
		def.setTargets(in.Board.SpaceID, in.Board.BoardID)
	}
	name := in.Name
	if strings.TrimSpace(name) == "" {
		name = t.Name
	}
	var grants []access.AppGrant
	// A private space has one member, and so nothing to restrict.
	if t.Roles != nil && sp.Kind == access.Shared {
		me := a.UserID
		grants = append(grants, access.AppGrant{UserID: &me, Roles: slices.Clone(t.Roles.Installer)})
		if len(t.Roles.Members) > 0 {
			grants = append(grants, access.AppGrant{Roles: slices.Clone(t.Roles.Members)})
		}
	}
	return s.createWithRoles(ctx, sp, a, NewApp{Name: name, Definition: def}, grants)
}

// chooseBoard checks that a may write the board ref names, in this workspace.
func (s *Service) chooseBoard(ctx context.Context, sp access.Space, a Actor, ref BoardRef) error {
	if ref.SpaceID < 1 || ref.BoardID < 1 {
		return invalidAs(apierr.CodeAppTemplateBoard, nil, "space_id and board_id are IDs, from 1")
	}
	target, err := s.access.Check(ctx, a.UserID, ref.SpaceID)
	if errors.Is(err, access.ErrNotFound) {
		return s.refusedTarget(ref.SpaceID)
	}
	if err != nil {
		return err
	}
	if target.WorkspaceID != sp.WorkspaceID {
		return s.refusedTarget(ref.SpaceID)
	}
	return s.targetOfDefinition(ctx, sp.WorkspaceID, a, ref.SpaceID, ref.BoardID, true)
}

func (s *Service) refusedTarget(spaceID int64) error {
	return invalidAs(apierr.CodeAppEffectTarget, apierr.Params{"space_id": spaceID},
		"an effect targets space %d, which you can't write: choose a space and board you can", spaceID)
}

// DuplicateRequest asks for a copy of an app's definition.
type DuplicateRequest struct {
	// Name is the copy's name; the source's if left out.
	Name string `json:"name"`
	// SpaceID is the space of the copy: the source's if left out. The caller must be able to
	// write it, and it must be in the same workspace.
	SpaceID int64 `json:"space_id"`
	// Source is which definition is copied: "live" (the release people use) or "draft" (what is
	// being edited, for those who may design or publish the app). Left out, it is the draft for
	// those and the live release for everyone else who may view the app.
	Source string `json:"source"`
}

// duplicateApp copies the definition of appID of sp into a new app, for a caller who may view
// the source and write the destination. Of the source it reads the definition they are allowed to
// see and nothing else.
func (s *Service) duplicateApp(ctx context.Context, sp access.Space, a Actor, appID int64, req DuplicateRequest) (App, error) {
	if err := may(sp, access.Read); err != nil {
		return App{}, err
	}
	dest := sp
	if req.SpaceID != 0 && req.SpaceID != sp.ID {
		var err error
		if dest, err = s.access.Check(ctx, a.UserID, req.SpaceID); errors.Is(err, access.ErrNotFound) {
			return App{}, ErrNotFound
		} else if err != nil {
			return App{}, err
		}
		if dest.WorkspaceID != sp.WorkspaceID {
			return App{}, ErrNotFound
		}
	}
	if err := may(dest, access.Write); err != nil {
		return App{}, err
	}
	// The source is as unseen to someone who holds no role in it as to someone outside its space.
	acc, err := s.appAccess(ctx, a, sp.ID, appID)
	if err != nil {
		return App{}, err
	}
	src, err := app(ctx, s.db, sp.ID, appID)
	if err != nil {
		return App{}, err
	}
	var from Definition
	switch req.Source {
	case "", "draft", "live":
		which := req.Source
		if which == "" {
			which = "live"
			if reviews(acc) {
				which = "draft"
			}
		}
		if which == "draft" {
			if !reviews(acc) {
				return App{}, forbidden(string(access.CapDesign))
			}
			from = src.draftDef
		} else {
			if src.liveDef == nil {
				return App{}, ErrNotPublished
			}
			from = *src.liveDef
		}
	default:
		return App{}, invalidf(`source must be "live" or "draft"`)
	}
	def, err := cloneDefinition(from)
	if err != nil {
		return App{}, err
	}
	// What the copier can't write isn't theirs to aim an app at: those targets are cleared, and the
	// copy can't be published until they choose again (unsetTargets).
	if err := s.clearForeignTargets(ctx, dest.WorkspaceID, a, &def); err != nil {
		return App{}, err
	}
	name := req.Name
	if strings.TrimSpace(name) == "" {
		name = src.Name
	}
	// No roles: the copy is the copier's to set up. An unrestricted app is what the space alone
	// allows, which is never more than the source's members could do to it.
	return s.createWithRoles(ctx, dest, a, NewApp{Name: name, Definition: def}, nil)
}

// clearForeignTargets clears the target of every effect of def that a may not use: one they can't
// write (a card or a document) or read (a route), or whose space is in another workspace. An
// effect's name and templates stay, so choosing a board again is all that's left.
func (s *Service) clearForeignTargets(ctx context.Context, workspaceID int64, a Actor, def *Definition) error {
	for name, act := range def.Actions {
		effects := slices.Clone(act.Effects)
		for i, e := range effects {
			if e.SpaceID == 0 {
				continue
			}
			write := e.Kind == EffectCreateCard || e.Kind == EffectCreateDocument
			err := s.targetOfDefinition(ctx, workspaceID, a, e.SpaceID, e.BoardID, write)
			if err == nil {
				continue
			}
			if !errors.Is(err, ErrInvalid) {
				return err
			}
			effects[i].SpaceID, effects[i].BoardID = 0, 0
		}
		act.Effects = effects
		def.Actions[name] = act
	}
	return nil
}

// createWithRoles is createApp with the first role assignments made in the same transaction.
func (s *Service) createWithRoles(ctx context.Context, sp access.Space, a Actor, na NewApp, grants []access.AppGrant) (App, error) {
	return s.createAppWith(ctx, sp, a, na, grants)
}

// unsetError is the refusal to publish a draft that has effects with no board chosen.
func unsetError(actions []string) error {
	return invalidAs(apierr.CodeAppEffectTargetUnset, apierr.Params{"action": actions[0]},
		"the effect of %q has no board chosen yet: choose one you can write before publishing", actions[0])
}

// chooseTargets sets the board of every effect of the draft that has none, for a caller who may
// design the app and write the board.
func (s *Service) chooseTargets(ctx context.Context, sp access.Space, a Actor, appID, version int64, ref BoardRef) (App, error) {
	if err := may(sp, access.Write); err != nil {
		return App{}, err
	}
	acc, err := s.appAccess(ctx, a, sp.ID, appID)
	if err != nil {
		return App{}, err
	}
	if err := need(acc, access.CapDesign); err != nil {
		return App{}, err
	}
	if err := s.chooseBoard(ctx, sp, a, ref); err != nil {
		return App{}, err
	}
	cur, err := app(ctx, s.db, sp.ID, appID)
	if err != nil {
		return App{}, err
	}
	def, err := cloneDefinition(cur.draftDef)
	if err != nil {
		return App{}, err
	}
	if def.setTargets(ref.SpaceID, ref.BoardID) == 0 {
		return App{}, invalidf("no effect of this app is waiting for a board")
	}
	// updateApp checks the version, the right to design and the targets again, under the lock.
	return s.updateApp(ctx, sp, a, appID, Changes{Version: version, Definition: &def})
}

// TemplateRoutes mounts the template list, behind a's RequireMember: the templates are the
// server's, the same for everyone in a workspace. Installing and duplicating are in Routes, under
// the space they write.
func (s *Service) TemplateRoutes(mux *http.ServeMux, a *auth.Service) {
	mux.Handle("GET /api/workspaces/{workspace}/app-templates", a.RequireMember(http.HandlerFunc(s.handleTemplates)))
}

func (s *Service) handleTemplates(w http.ResponseWriter, r *http.Request) {
	list, err := TemplateInfos()
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]TemplateInfo{"templates": list})
}

func (s *Service) handleInstall(w http.ResponseWriter, r *http.Request) {
	req, _ := parse(w, r, false)
	var in Install
	if !readJSON(w, r, &in) {
		return
	}
	a, err := s.installTemplate(r.Context(), req.space, req.actor, r.PathValue("template"), in)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Service) handleDuplicate(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	var in DuplicateRequest
	if !readJSON(w, r, &in) {
		return
	}
	a, err := s.duplicateApp(r.Context(), req.space, req.actor, req.app, in)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Service) handleChooseBoard(w http.ResponseWriter, r *http.Request) {
	req, ok := parse(w, r, true)
	if !ok {
		return
	}
	var in struct {
		Version int64 `json:"version"`
		BoardRef
	}
	if !readJSON(w, r, &in) {
		return
	}
	a, err := s.chooseTargets(r.Context(), req.space, req.actor, req.app, in.Version, in.BoardRef)
	s.result(w, r, req, a, err)
}
