package apps

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

// The draft's preview: the app's draft drawn and used on sample rows that live in the caller's
// browser, never in the database. People who may design or publish an app try its draft here
// before they publish it, and what they do changes nothing real: no row is read from or written
// to the app's data, nothing is recorded in a record's history and nothing else an action could
// do happens. The preview holds no state: the caller sends the rows as they are, and the answer
// has them as the action leaves them. Service.preview doesn't take a transaction or the write
// lock, and the preview tests check, against the database, that nothing is written.

// previewMaxRows bounds the sample rows a preview holds, so a request is a reasonable size.
const previewMaxRows = 100

// previewMaxBody bounds a preview's request, which carries every sample row: as many as an app may
// hold in all, with room for the JSON around them and for the escaping of their text.
const previewMaxBody = 2*MaxDataSize + 64<<10

// previewFull is a preview at previewMaxRows. It says the preview's limit, not the live app's.
func previewFull() error {
	return &fullError{
		msg:  fmt.Sprintf("a preview holds at most %d rows", previewMaxRows),
		code: apierr.CodeAppPreviewFull, params: apierr.Params{"max_rows": previewMaxRows},
	}
}

// sampleRowCount is how many sample rows a preview starts with.
const sampleRowCount = 3

// PreviewRow is one sample row of a preview: as a stored row, but simulated.
type PreviewRow struct {
	ID       int64          `json:"id"`
	Revision int64          `json:"revision"`
	Data     map[string]any `json:"data"`
}

// PreviewRequest is a step of a preview. Without Rows it starts one, on sample rows made from the
// draft's collection; with them it goes on from where the caller is. Action, if any, is run on the
// rows as the draft defines it.
type PreviewRequest struct {
	Action *ClientAction `json:"action,omitempty"`
	Rows   []PreviewRow  `json:"rows"`
}

// PreviewReply is the messages that draw or update the screen, and the sample rows as they are
// now, to send back with the next step.
type PreviewReply struct {
	Messages []Message    `json:"messages"`
	Rows     []PreviewRow `json:"rows"`
	// Effects are what the action would have set going on boards and documents, simulated on
	// the same sample rows (nothing is made); left out when there are none.
	Effects []EffectView `json:"effects,omitempty"`
}

// SampleRows makes the sample rows of a collection: a few rows of plausible values of every field,
// the same on every call. A row the collection's rules refuse is left out.
func SampleRows(c Collection) []Row {
	c = c.forPreview()
	var rows []Row
	for n := 1; n <= sampleRowCount; n++ {
		data := map[string]any{}
		for _, f := range c.Fields {
			if v, ok := sampleValue(f, n); ok {
				data[f.Name] = v
			}
		}
		if c.Fits(data) == nil {
			rows = append(rows, Row{ID: int64(len(rows) + 1), Revision: 1, Data: data})
		}
	}
	return rows
}

// sampleValue is the n-th sample value of a field, within its limits.
func sampleValue(f Field, n int) (any, bool) {
	switch f.Type {
	case TypeString:
		s := fmt.Sprintf("%s %d", strings.ToUpper(f.Name[:1])+strings.ReplaceAll(f.Name[1:], "_", " "), n)
		if f.MaxLength > 0 && utf8.RuneCountInString(s) > f.MaxLength {
			s = string([]rune(s)[:f.MaxLength])
		}
		return s, true
	case TypeNumber:
		v := float64(n * 10)
		if f.Min != nil && v < *f.Min {
			v = *f.Min
		}
		if f.Max != nil && v > *f.Max {
			v = *f.Max
		}
		return v, true
	case TypeBoolean:
		return n%2 == 1, true
	case TypeDate:
		return fmt.Sprintf("2026-01-%02d", 9+n), true
	case TypeTime:
		return fmt.Sprintf("%02d:30", 8+n), true
	case TypeChoice:
		if len(f.Options) == 0 {
			return nil, false
		}
		return f.Options[(n-1)%len(f.Options)], true
	case TypeLocation:
		// A place and a time that are plain sample data, not a person's: the same on every call.
		return map[string]any{"lat": 38.7 + float64(n)/100, "lon": -9.1, "accuracy_m": 10.0,
			"captured_at": fmt.Sprintf("2026-01-%02dT09:30:00.000Z", 9+n), "source": SourceDevice}, true
	case TypeBarcode:
		return sampleCode(f, n), true
	}
	// A signature (like a photo) has no sample: a preview never holds a picture.
	return nil, false
}

// sampleCode is the n-th sample text of a barcode field: a number with the right check digit for a
// format the server checks, text otherwise, within the field's length.
func sampleCode(f Field, n int) string {
	// digits is a number of the length, ending in its check digit if the format has one.
	digits := func(length int, check bool) string {
		if !check {
			return fmt.Sprintf("%0*d", length, 400638+n)
		}
		body := fmt.Sprintf("%0*d", length-1, 400638133000+n)
		body = body[len(body)-(length-1):]
		for d := range 10 {
			if checkDigitOK(body + fmt.Sprint(d)) {
				return body + fmt.Sprint(d)
			}
		}
		return body + "0"
	}
	s := fmt.Sprintf("CODE-%d", 1000+n)
	switch f.Format {
	case "ean_13":
		s = digits(13, true)
	case "ean_8":
		s = digits(8, true)
	case "upc_a":
		s = digits(12, true)
	case "upc_e":
		s = digits(6, false)
	}
	if f.MaxLength > 0 && utf8.RuneCountInString(s) > f.MaxLength {
		s = string([]rune(s)[:f.MaxLength])
	}
	return s
}

// forPreview is the collection as a preview holds it: the same, but a field that holds a picture is never
// required, since a preview holds none (it would otherwise refuse every row and every action).
func (c Collection) forPreview() Collection {
	out := Collection{Fields: slices.Clone(c.Fields)}
	for i, f := range out.Fields {
		if f.Type == TypeAttachment || f.Type == TypeSignature {
			out.Fields[i].Required, out.Fields[i].RequiredWhen = false, nil
		}
	}
	return out
}

// previewDefinition is def as a preview plans its actions: with the collection a preview holds
// (forPreview), so that an action whose record would carry a photo or a signature isn't refused
// for the lack of one.
func previewDefinition(def Definition) Definition {
	def.Collection = def.Collection.forPreview()
	return def
}

// preview draws the app's draft, or runs one of its actions, on sample rows. It is for those who
// may design or publish the app (the draft is theirs to see), and reads nothing of the app's data.
func (s *Service) preview(ctx context.Context, sp access.Space, actor Actor, appID int64, req PreviewRequest) (PreviewReply, error) {
	if _, err := s.reviewer(ctx, sp, actor, appID); err != nil {
		return PreviewReply{}, err
	}
	a, err := launchable(ctx, s.db, sp.ID, appID)
	if err != nil {
		return PreviewReply{}, err
	}
	def := a.draftDef
	surfaceID := SurfaceID(a.ID)

	var rows []Row
	if req.Rows == nil {
		rows = SampleRows(def.Collection)
	} else if rows, err = previewRows(def, req.Rows); err != nil {
		return PreviewReply{}, err
	}
	if req.Action == nil {
		return PreviewReply{Messages: Surface(surfaceID, def, rows), Rows: previewReply(rows)}, nil
	}

	ch, err := Plan(surfaceID, previewDefinition(def), *req.Action)
	if err != nil {
		return PreviewReply{}, err
	}
	if ch.Kind == ActionNavigate {
		record := map[string]any{}
		if ch.ID != 0 {
			r, ok := findRow(rows, ch.ID)
			if !ok {
				return PreviewReply{}, invalidAs(apierr.CodeAppRowNotFound, nil, "the app has no row %d", ch.ID)
			}
			record = item(def, r)
		}
		return PreviewReply{Messages: Opened(surfaceID, ch.Screen, record), Rows: previewReply(rows)}, nil
	}
	if rows, err = simulate(def, rows, ch); err != nil {
		return PreviewReply{}, err
	}
	reply := PreviewReply{Messages: answer(a.ID, def, rows, ch), Rows: previewReply(rows)}
	if ch.Kind == ActionInsert || ch.Kind == ActionUpdate {
		reply.Effects = s.simulatedEffects(ctx, actor.UserID, def, ch, rows)
	}
	return reply, nil
}

// simulate applies change to rows as the live app would to its own, in memory: the rows it
// returns are new, the ones it was given are left as they were.
func simulate(def Definition, rows []Row, ch Change) ([]Row, error) {
	// A preview has no photos: an id in an action names an attachment of a live app, which a
	// preview never reads or holds.
	ch.Data = withoutPhotos(def, ch.Data)
	switch ch.Kind {
	case ActionInsert:
		if len(rows) >= previewMaxRows {
			return nil, previewFull()
		}
		if err := def.Collection.forPreview().Fits(ch.Data); err != nil {
			return nil, err
		}
		var id int64
		for _, r := range rows {
			id = max(id, r.ID)
		}
		return append(append([]Row{}, rows...), Row{ID: id + 1, Revision: 1, Data: declared(def, ch.Data)}), nil
	case ActionUpdate, ActionDelete:
		cur, ok := findRow(rows, ch.ID)
		if !ok {
			return nil, invalidAs(apierr.CodeAppRowNotFound, nil, "the app has no row %d", ch.ID)
		}
		if ch.Expected != nil && *ch.Expected != cur.Revision {
			return nil, &conflictError{id: ch.ID, current: item(def, cur)}
		}
		out := make([]Row, 0, len(rows))
		for _, r := range rows {
			switch {
			case r.ID != ch.ID:
				out = append(out, r)
			case ch.Kind == ActionUpdate:
				next, err := def.Collection.forPreview().Merge(r.Data, ch.Data)
				if err != nil {
					return nil, err
				}
				out = append(out, Row{ID: r.ID, Revision: r.Revision + 1, Data: next})
			}
		}
		return out, nil
	}
	return nil, invalidf("unknown kind of action %q", ch.Kind)
}

// withoutPhotos is data without its attachment and signature fields' values: a preview never holds an attachment id,
// so it can't draw, return or tell whether a real attachment exists. The sample rows have none.
func withoutPhotos(def Definition, data map[string]any) map[string]any {
	out := make(map[string]any, len(data))
	for k, v := range data {
		if f, ok := def.Collection.field(k); ok && (f.Type == TypeAttachment || f.Type == TypeSignature) {
			continue
		}
		out[k] = v
	}
	return out
}

func findRow(rows []Row, id int64) (Row, bool) {
	for _, r := range rows {
		if r.ID == id {
			return r, true
		}
	}
	return Row{}, false
}

// previewRows checks the rows a caller sends back against the draft: not too many, each with an
// ID of its own, and each fitting the collection. A row of a draft that has changed since (a field
// it requires and the row lacks) is refused, and the caller starts the preview again.
func previewRows(def Definition, given []PreviewRow) ([]Row, error) {
	if len(given) > previewMaxRows {
		return nil, previewFull()
	}
	seen := map[int64]bool{}
	rows := make([]Row, 0, len(given))
	for _, g := range given {
		if g.ID < 1 || g.Revision < 1 || seen[g.ID] {
			return nil, invalidf("a preview's rows each have an id and a revision, from 1, and an id of their own")
		}
		seen[g.ID] = true
		data := withoutPhotos(def, declared(def, g.Data))
		if err := def.Collection.forPreview().Fits(data); err != nil {
			return nil, err
		}
		rows = append(rows, Row{ID: g.ID, Revision: g.Revision, Data: data})
	}
	return rows, nil
}

func previewReply(rows []Row) []PreviewRow {
	out := make([]PreviewRow, len(rows))
	for i, r := range rows {
		out[i] = PreviewRow(r)
	}
	return out
}
