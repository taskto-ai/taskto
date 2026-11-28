package apps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

type Release struct {
	Number            int64       `json:"number"`
	DefinitionVersion int         `json:"definition_version"`
	Requires          []string    `json:"requires" apitypes:"nonnull"`
	PublishedAt       string      `json:"published_at"`
	PublishedBy       *Author     `json:"published_by"`
	Live              bool        `json:"live"`
	LiveSince         string      `json:"live_since,omitempty"`
	Definition        *Definition `json:"definition,omitempty"`
}

var (
	ErrNotPublished        = errors.New("this app is not published yet: it has a draft, but no release to launch")
	ErrReleaseNotFound     = errors.New("the app has no such release")
	ErrReleaseOutdated     = errors.New("this screen is from an older release of the app: reload it")
	ErrReleaseIncompatible = errors.New("the app's data doesn't fit that release")
	ErrReleaseLive         = errors.New("that release is already live")
	ErrNothingToPublish    = errors.New("the draft is the live release: there is nothing to publish")
	ErrMigration           = errors.New("the schema changes aren't acknowledged")
)

type outdatedError struct{ release, live int64 }

func (e *outdatedError) Error() string        { return ErrReleaseOutdated.Error() }
func (e *outdatedError) Is(target error) bool { return target == ErrReleaseOutdated }

type incompatibleError struct{ fields []string }

func (e *incompatibleError) Error() string {
	return fmt.Sprintf("%v: %v", ErrReleaseIncompatible, e.fields)
}
func (e *incompatibleError) Is(target error) bool { return target == ErrReleaseIncompatible }

type migrationError struct {
	unacknowledged bool
	changes        []SchemaChange
}

func (e *migrationError) Error() string {
	if e.unacknowledged {
		return fmt.Sprintf("the draft changes the collection in ways that need acknowledging: %v", e.changes)
	}
	return fmt.Sprintf("these acknowledged changes are not made by the draft: %v", e.changes)
}
func (e *migrationError) Is(target error) bool { return target == ErrMigration }

const (
	ChangeFieldRemoved      = "field_removed"
	ChangeFieldTypeChanged  = "field_type_changed"
	ChangeFieldRequired     = "field_required"
	ChangeFieldNarrowed     = "field_narrowed"
	ChangeFieldRequiredWhen = "field_required_when"
)

var schemaChanges = []string{ChangeFieldRemoved, ChangeFieldTypeChanged, ChangeFieldRequired, ChangeFieldNarrowed, ChangeFieldRequiredWhen}

type SchemaChange struct {
	Change string `json:"change"`
	Field  string `json:"field"`
}

type Publish struct {
	Version     int64          `json:"version"`
	Acknowledge []SchemaChange `json:"acknowledge"`
}

type Rollback struct {
	Version int64 `json:"version"`
	Release int64 `json:"release"`
}

func newRelease(number int64, version int, requires, at string, by sql.NullInt64, byName string) (Release, error) {
	rel := Release{Number: number, DefinitionVersion: version, Requires: []string{}, PublishedAt: at, PublishedBy: author(by, byName, false, sql.NullString{})}
	if err := json.Unmarshal([]byte(requires), &rel.Requires); err != nil {
		return Release{}, fmt.Errorf("release %d: requires: %w", number, err)
	}
	if rel.Requires == nil {
		rel.Requires = []string{}
	}
	return rel, nil
}

func decodeDefinition(text string, version int, requires []string) (Definition, error) {
	var d Definition
	if err := json.Unmarshal([]byte(text), &d); err != nil {
		return Definition{}, err
	}
	if d.DefinitionVersion == 0 {
		d.DefinitionVersion = max(version, 1)
	}
	if d.Requires == nil {
		d.Requires = requires
	}
	if d.Requires == nil {
		d.Requires = []string{}
	}
	return d, nil
}

func sameDefinition(a, b Definition) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	var vx, vy any
	if json.Unmarshal(x, &vx) != nil || json.Unmarshal(y, &vy) != nil {
		return false
	}
	return reflect.DeepEqual(vx, vy)
}

func sameCollection(a, b Collection) bool {
	return sameDefinition(Definition{Collection: withoutPresentation(a)}, Definition{Collection: withoutPresentation(b)})
}

func withoutPresentation(c Collection) Collection {
	c.Fields = slices.Clone(c.Fields)
	for i := range c.Fields {
		c.Fields[i].OptionLabels = nil
	}
	return c
}

const releaseFrom = `
	FROM app_releases r
	JOIN apps a ON a.id = r.app_id
	LEFT JOIN users u ON u.id = r.published_by`

const releaseFields = `r.number, r.definition_version, r.requires, r.published_at, r.published_by, coalesce(u.name, ''),
	r.number = a.live_release, coalesce(a.live_at, ''), `

func scanRelease(row scanner, withDefinition bool) (Release, error) {
	var number int64
	var version int
	var requires, at, liveSince, def string
	var by sql.NullInt64
	var byName string
	var live bool
	if err := row.Scan(&number, &version, &requires, &at, &by, &byName, &live, &liveSince, &def); err != nil {
		return Release{}, err
	}
	rel, err := newRelease(number, version, requires, at, by, byName)
	if err != nil {
		return Release{}, err
	}
	if live {
		rel.Live, rel.LiveSince = true, liveSince
	}
	if withDefinition {
		d, err := decodeDefinition(def, rel.DefinitionVersion, rel.Requires)
		if err != nil {
			return Release{}, fmt.Errorf("release %d: stored definition: %w", number, err)
		}
		rel.Definition = &d
	}
	return rel, nil
}

func loadRelease(ctx context.Context, q queryer, appID, number int64, withDefinition bool) (Release, error) {
	col := `''`
	if withDefinition {
		col = `r.definition`
	}
	rel, err := scanRelease(q.QueryRowContext(ctx, `SELECT `+releaseFields+col+releaseFrom+` WHERE r.app_id = ? AND r.number = ?`, appID, number), withDefinition)
	if errors.Is(err, sql.ErrNoRows) {
		return Release{}, ErrReleaseNotFound
	}
	return rel, err
}

func (s *Service) reviewer(ctx context.Context, sp access.Space, a Actor, appID int64) (access.AppAccess, error) {
	if err := may(sp, access.Read); err != nil {
		return access.AppAccess{}, err
	}
	acc, err := s.appAccess(ctx, a, sp.ID, appID)
	if err != nil {
		return access.AppAccess{}, err
	}
	if !reviews(acc) {
		return access.AppAccess{}, forbidden(string(access.CapDesign))
	}
	return acc, nil
}

func (s *Service) listReleases(ctx context.Context, sp access.Space, a Actor, appID int64) ([]Release, error) {
	if _, err := s.reviewer(ctx, sp, a, appID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+releaseFields+`''`+releaseFrom+` WHERE r.app_id = ? AND a.space_id = ? ORDER BY r.number DESC`, appID, sp.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Release{}
	for rows.Next() {
		rel, err := scanRelease(rows, false)
		if err != nil {
			return nil, err
		}
		all = append(all, rel)
	}
	return all, rows.Err()
}

func (s *Service) getRelease(ctx context.Context, sp access.Space, a Actor, appID, number int64) (Release, error) {
	if _, err := s.reviewer(ctx, sp, a, appID); err != nil {
		return Release{}, err
	}
	return loadRelease(ctx, s.db, appID, number, true)
}

func screenOf(ctx context.Context, q queryer, a App, acc access.AppAccess, which string) (Definition, error) {
	switch which {
	case "":
		if a.liveDef == nil {
			return Definition{}, ErrNotPublished
		}
		return *a.liveDef, nil
	case "draft":
		if !reviews(acc) {
			return Definition{}, forbidden(string(access.CapDesign))
		}
		return a.draftDef, nil
	}
	n, err := strconv.ParseInt(which, 10, 64)
	if err != nil || n < 1 {
		return Definition{}, invalidf(`release must be "draft" or a release number`)
	}
	if a.Live != nil && a.Live.Number == n {
		return *a.liveDef, nil
	}
	if !reviews(acc) {
		return Definition{}, forbidden(string(access.CapDesign))
	}
	rel, err := loadRelease(ctx, q, a.ID, n, true)
	if err != nil {
		return Definition{}, err
	}
	return *rel.Definition, nil
}

func (s *Service) publishApp(ctx context.Context, sp access.Space, a Actor, appID int64, req Publish) (App, error) {
	if err := may(sp, access.Write); err != nil {
		return App{}, err
	}
	if err := checkVersion(req.Version); err != nil {
		return App{}, err
	}
	if err := checkAcknowledge(req.Acknowledge); err != nil {
		return App{}, err
	}
	var published App
	err := s.write(ctx, func(tx *sql.Tx) error {
		acc, err := s.recheckApp(ctx, a, sp.ID, appID)
		if err != nil {
			return err
		}
		if err := need(acc, access.CapPublish); err != nil {
			return err
		}
		cur, err := app(ctx, tx, sp.ID, appID)
		if err != nil {
			return err
		}
		if unset := cur.draftDef.unsetTargets(); len(unset) > 0 {
			return unsetError(unset)
		}
		if err := s.checkEffectTargets(ctx, sp.WorkspaceID, a, cur.draftDef); err != nil {
			return err
		}
		if cur.Version != req.Version {
			return ErrStale
		}
		checked, err := cur.draftDef.Check()
		if err != nil {
			return err
		}
		if err := s.checkApprovals(ctx, tx, appID, checked); err != nil {
			return err
		}
		var base *Collection
		if cur.liveDef != nil {
			if sameDefinition(*cur.liveDef, checked) {
				return ErrNothingToPublish
			}
			base = &cur.liveDef.Collection
		}
		blind := acc.ReadRecords != access.ScopeAll
		var rows []Row
		if !blind {
			if rows, err = loadRows(ctx, tx, appID, everything); err != nil {
				return err
			}
		}
		if err := acknowledged(breakingChanges(base, checked.Collection, rows, blind), req.Acknowledge); err != nil {
			return err
		}
		if err := paymentMoneyKept(ctx, tx, s.platform.Payments, appID, cur.liveDef, checked); err != nil {
			return err
		}
		raw, err := json.Marshal(checked)
		if err != nil {
			return err
		}
		requires, err := json.Marshal(checked.Requires)
		if err != nil {
			return err
		}
		var number int64
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(number), 0) + 1 FROM app_releases WHERE app_id = ?`, appID).Scan(&number); err != nil {
			return err
		}
		stamp := now()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO app_releases (app_id, number, definition, definition_version, requires, published_at, published_by)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, appID, number, string(raw), checked.DefinitionVersion, string(requires), stamp, a.UserID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE apps SET definition = ?, live_release = ?, live_at = ?, draft_differs = 0,
				version = version + 1, updated_at = ?, updated_by = ?, updated_by_agent = ?, updated_via = ?
			WHERE id = ?`, string(raw), number, stamp, stamp, a.UserID, a.Agent, via(a), appID); err != nil {
			return err
		}
		if err := s.syncSchedules(ctx, tx, appID, a.UserID); err != nil {
			return err
		}
		published, err = app(ctx, tx, sp.ID, appID)
		return err
	})
	if err != nil {
		return App{}, err
	}
	if err := s.present(ctx, a, &published); err != nil {
		return App{}, err
	}
	return published, nil
}

func (s *Service) rollbackApp(ctx context.Context, sp access.Space, a Actor, appID int64, req Rollback) (App, error) {
	if err := may(sp, access.Write); err != nil {
		return App{}, err
	}
	if err := checkVersion(req.Version); err != nil {
		return App{}, err
	}
	if req.Release < 1 {
		return App{}, invalidf("release is required: the number of the release to roll back to")
	}
	var rolled App
	err := s.write(ctx, func(tx *sql.Tx) error {
		acc, err := s.recheckApp(ctx, a, sp.ID, appID)
		if err != nil {
			return err
		}
		if err := need(acc, access.CapPublish); err != nil {
			return err
		}
		cur, err := app(ctx, tx, sp.ID, appID)
		if err != nil {
			return err
		}
		if cur.Version != req.Version {
			return ErrStale
		}
		target, err := loadRelease(ctx, tx, appID, req.Release, true)
		if err != nil {
			return err
		}
		if cur.Live != nil && cur.Live.Number == target.Number {
			return ErrReleaseLive
		}
		if err := s.checkEffectTargets(ctx, sp.WorkspaceID, a, *target.Definition); err != nil {
			return err
		}
		if err := s.checkApprovals(ctx, tx, appID, *target.Definition); err != nil {
			return err
		}
		if cur.liveDef == nil || !sameCollection(cur.liveDef.Collection, target.Definition.Collection) {
			if acc.ReadRecords != access.ScopeAll {
				return forbidden(capReadRecords)
			}
			rows, err := loadRows(ctx, tx, appID, everything)
			if err != nil {
				return err
			}
			if fields := misfits(target.Definition.Collection, rows); len(fields) > 0 {
				return &incompatibleError{fields: fields}
			}
		}
		if err := paymentMoneyKept(ctx, tx, s.platform.Payments, appID, cur.liveDef, *target.Definition); err != nil {
			return err
		}
		stamp := now()
		if _, err := tx.ExecContext(ctx, `
			UPDATE apps SET live_release = ?, live_at = ?, draft_differs = ?,
				version = version + 1, updated_at = ?, updated_by = ?, updated_by_agent = ?, updated_via = ?
			WHERE id = ?`, target.Number, stamp, !sameDefinition(cur.draftDef, *target.Definition), stamp, a.UserID, a.Agent, via(a), appID); err != nil {
			return err
		}
		if err := s.syncSchedules(ctx, tx, appID, a.UserID); err != nil {
			return err
		}
		rolled, err = app(ctx, tx, sp.ID, appID)
		return err
	})
	if err != nil {
		return App{}, err
	}
	if err := s.present(ctx, a, &rolled); err != nil {
		return App{}, err
	}
	return rolled, nil
}

func misfits(c Collection, rows []Row) []string {
	var bad []string
	for _, f := range c.Fields {
		if slices.ContainsFunc(rows, func(r Row) bool { return c.fitsField(f, r.Data) != nil }) {
			bad = append(bad, f.Name)
		}
	}
	return bad
}

func holds(r Row, name string) bool {
	v, ok := r.Data[name]
	return ok && v != nil
}

func breakingChanges(live *Collection, next Collection, rows []Row, blind bool) []SchemaChange {
	if live == nil {
		return nil
	}
	var out []SchemaChange
	anyRow := func(test func(Row) bool) bool { return blind || slices.ContainsFunc(rows, test) }
	for _, was := range live.Fields {
		nf, kept := next.field(was.Name)
		if !kept {
			if anyRow(func(r Row) bool { return holds(r, was.Name) }) {
				out = append(out, SchemaChange{ChangeFieldRemoved, was.Name})
			}
			continue
		}
		retyped := nf.Type != was.Type
		intoEvidence := retyped && (nf.Type == TypeLocation || nf.Type == TypeSignature)
		if retyped && (intoEvidence || anyRow(func(r Row) bool { return holds(r, was.Name) })) {
			out = append(out, SchemaChange{ChangeFieldTypeChanged, was.Name})
		}
		if nf.Required && !was.Required && anyRow(func(r Row) bool { return !holds(r, was.Name) }) {
			out = append(out, SchemaChange{ChangeFieldRequired, was.Name})
		}
		if !retyped && (blind && narrower(was, nf) || !blind && anyRow(func(r Row) bool {
			v, ok := r.Data[was.Name]
			if !ok || v == nil {
				return false
			}
			_, _, errNow := nf.parse(v)
			_, _, errWas := was.parse(v)
			return errNow != nil && errWas == nil
		})) {
			out = append(out, SchemaChange{ChangeFieldNarrowed, was.Name})
		}
		if !nf.Required && nf.RequiredWhen != nil && !sameRule(nf.RequiredWhen, was.RequiredWhen) && anyRow(func(r Row) bool {
			return !holds(r, was.Name) && next.requires(nf, r.Data) && !live.requires(was, r.Data)
		}) {
			out = append(out, SchemaChange{ChangeFieldRequiredWhen, was.Name})
		}
	}
	for _, f := range next.Fields {
		if _, existed := live.field(f.Name); existed {
			continue
		}
		if f.Required && anyRow(func(r Row) bool { return !holds(r, f.Name) }) {
			out = append(out, SchemaChange{ChangeFieldRequired, f.Name})
		}
		if !f.Required && f.RequiredWhen != nil && anyRow(func(r Row) bool { return !holds(r, f.Name) && next.requires(f, r.Data) }) {
			out = append(out, SchemaChange{ChangeFieldRequiredWhen, f.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Field != out[j].Field {
			return out[i].Field < out[j].Field
		}
		return out[i].Change < out[j].Change
	})
	return out
}

func narrower(was, next Field) bool {
	length := func(f Field) int {
		if f.MaxLength == 0 {
			return DefaultMaxLength
		}
		return f.MaxLength
	}
	if length(next) < length(was) {
		return true
	}
	if was.Type == TypeChoice {
		for _, o := range was.Options {
			if !slices.Contains(next.Options, o) {
				return true
			}
		}
	}
	if was.Type == TypeAttachment && next.MaxFiles < was.MaxFiles {
		return true
	}
	if was.Type == TypeBarcode && next.Format != "" && next.Format != was.Format {
		return true
	}
	if next.Min != nil && (was.Min == nil || *next.Min > *was.Min) {
		return true
	}
	return next.Max != nil && (was.Max == nil || *next.Max < *was.Max)
}

func checkAcknowledge(list []SchemaChange) error {
	seen := map[SchemaChange]bool{}
	for _, c := range list {
		if !slices.Contains(schemaChanges, c.Change) || !namePattern.MatchString(c.Field) || seen[c] {
			return invalidAs(apierr.CodeAppMigrationInvalid, nil,
				"acknowledge: each entry is {\"change\": one of %v, \"field\": a field's name}, once", schemaChanges)
		}
		seen[c] = true
	}
	return nil
}

func acknowledged(found, ack []SchemaChange) error {
	var missing, extra []SchemaChange
	for _, c := range found {
		if !slices.Contains(ack, c) {
			missing = append(missing, c)
		}
	}
	for _, c := range ack {
		if !slices.Contains(found, c) {
			extra = append(extra, c)
		}
	}
	if len(missing) > 0 {
		return &migrationError{unacknowledged: true, changes: found}
	}
	if len(extra) > 0 {
		return &migrationError{changes: extra}
	}
	return nil
}

func liveTakes(live, from Definition, name string, ch Change) bool {
	la, ok := live.Actions[name]
	if !ok {
		return false
	}
	fa := from.Actions[name]
	la.Effects, fa.Effects = nil, nil
	if !reflect.DeepEqual(la, fa) {
		return false
	}
	for field := range ch.Data {
		lf, inLive := live.Collection.field(field)
		ff, _ := from.Collection.field(field)
		if !inLive || lf.Type != ff.Type {
			return false
		}
	}
	if ch.Kind == ActionInsert {
		for _, f := range live.Collection.Fields {
			if _, given := ch.Data[f.Name]; f.Required && !given {
				return false
			}
		}
	}
	return true
}
