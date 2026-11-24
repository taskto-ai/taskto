package apps

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
)

// An app whose collection has two attachment fields, drawn with the photo components.
const photoDefinition = `{
  "components": [
    {"id": "root", "component": "Column", "children": ["title", "photo", "receipt", "add", "list"]},
    {"id": "title", "component": "TextField", "label": "Title", "value": {"path": "/draft/title"}},
    {"id": "photo", "component": "PhotoInput", "field": "photos", "label": "Photos", "value": {"path": "/draft/photos"}},
    {"id": "receipt", "component": "PhotoInput", "field": "receipt", "value": {"path": "/draft/receipt"}},
    {"id": "add_label", "component": "Text", "text": "Add"},
    {"id": "add", "component": "Button", "child": "add_label", "action": {"event": {"name": "add_note", "context": {
      "title": {"path": "/draft/title"}, "photos": {"path": "/draft/photos"}, "receipt": {"path": "/draft/receipt"}}}}},
    {"id": "list", "component": "List", "children": {"componentId": "item", "path": "/items"}},
    {"id": "item", "component": "Photos", "value": {"path": "photos"}}
  ],
  "collection": {"fields": [
    {"name": "title", "type": "string"},
    {"name": "photos", "type": "attachment", "max_files": 2, "max_bytes": 200000},
    {"name": "receipt", "type": "attachment"}
  ]},
  "actions": {"add_note": {"kind": "insert"}, "edit_note": {"kind": "update"}, "remove_note": {"kind": "delete"}}
}`

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

func contains(list []string, s string) bool { return slices.Contains(list, s) }

func photoBody(name string) map[string]any {
	var def map[string]any
	if err := jsonUnmarshal(photoDefinition, &def); err != nil {
		panic(err)
	}
	return map[string]any{"name": name, "definition": def}
}

// createPhotos makes and publishes an app with photoDefinition as u.
func (w *world) createPhotos(u *user, s access.Space, name string) App {
	w.e.t.Helper()
	rec := u.do(http.MethodPost, appsPath(s, ""), photoBody(name))
	expect(w.e.t, rec, http.StatusCreated)
	return w.publish(u, s, decode[App](w.e.t, rec))
}

// raw sends body as application/octet-stream, as a chunk is sent.
func (u *user) raw(method, path string, body []byte) *httptest.ResponseRecorder {
	u.e.t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if u.cookie != nil {
		r.AddCookie(u.cookie)
	}
	rec := httptest.NewRecorder()
	u.e.h.ServeHTTP(rec, r)
	return rec
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x * 255 / w & 0xff), uint8(y * 255 / h & 0xff), 99, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{200, uint8(y * 255 / h & 0xff), uint8(x * 255 / w & 0xff), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// withOrientation inserts an EXIF segment saying orientation o after a JPEG's start.
func withOrientation(jpg []byte, o int) []byte {
	var tiff bytes.Buffer
	tiff.WriteString("MM")
	_ = binary.Write(&tiff, binary.BigEndian, uint16(42))
	_ = binary.Write(&tiff, binary.BigEndian, uint32(8))
	_ = binary.Write(&tiff, binary.BigEndian, uint16(1))
	_ = binary.Write(&tiff, binary.BigEndian, uint16(0x0112))
	_ = binary.Write(&tiff, binary.BigEndian, uint16(3))
	_ = binary.Write(&tiff, binary.BigEndian, uint32(1))
	_ = binary.Write(&tiff, binary.BigEndian, uint16(o&0xffff))
	_ = binary.Write(&tiff, binary.BigEndian, uint16(0))
	_ = binary.Write(&tiff, binary.BigEndian, uint32(0))
	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	seg := []byte{0xff, 0xe1}
	seg = binary.BigEndian.AppendUint16(seg, uint16((len(payload)+2)&0xffff))
	seg = append(seg, payload...)
	return append(append(append([]byte{}, jpg[:2]...), seg...), jpg[2:]...)
}

func testGIF(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewPaletted(image.Rect(0, 0, 8, 6), color.Palette{color.White, color.Black})
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// pngClaiming is a PNG whose header claims w by h pixels (with a right checksum), and holds one.
func pngClaiming(t *testing.T, w, h uint32) []byte {
	t.Helper()
	b := testPNG(t, 1, 1)
	binary.BigEndian.PutUint32(b[16:], w)
	binary.BigEndian.PutUint32(b[20:], h)
	binary.BigEndian.PutUint32(b[29:], crc32.ChecksumIEEE(b[12:29]))
	return b
}

// upload is the API's three steps as u, with the chunk size given, returning the finalized
// attachment's id. It fails the test if a step doesn't succeed.
func (w *world) upload(u *user, s access.Space, a App, field string, data []byte, chunk int) string {
	w.e.t.Helper()
	id := w.start(u, s, a, field, len(data))
	w.send(u, s, a, id, data, chunk)
	rec := u.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+id+"/finalize"), map[string]any{})
	expect(w.e.t, rec, http.StatusOK)
	if got := decode[Attachment](w.e.t, rec); got.Status != "ready" || got.ContentType == "" {
		w.e.t.Fatalf("finalized = %+v", got)
	}
	return id
}

func (w *world) start(u *user, s access.Space, a App, field string, size int) string {
	w.e.t.Helper()
	rec := u.do(http.MethodPost, appPath(s, a.ID, "/uploads"), map[string]any{"field": field, "size": size})
	expect(w.e.t, rec, http.StatusCreated)
	up := decode[Attachment](w.e.t, rec)
	if up.Status != "uploading" || up.Received != 0 || up.ChunkSize == 0 || up.MaxChunk == 0 {
		w.e.t.Fatalf("created = %+v", up)
	}
	return up.ID
}

func (w *world) send(u *user, s access.Space, a App, id string, data []byte, chunk int) {
	w.e.t.Helper()
	for off := 0; off < len(data); off += chunk {
		rec := w.chunk(u, s, a, id, off, data[off:min(off+chunk, len(data))])
		expect(w.e.t, rec, http.StatusOK)
	}
}

func (w *world) chunk(u *user, s access.Space, a App, id string, offset int, data []byte) *httptest.ResponseRecorder {
	return u.raw(http.MethodPut, appPath(s, a.ID, fmt.Sprintf("/uploads/%s?offset=%d", id, offset)), data)
}

func (w *world) insert(u *user, s access.Space, a App, fields map[string]any) *httptest.ResponseRecorder {
	ctx := map[string]any{"title": "note"}
	for k, v := range fields {
		ctx[k] = v
	}
	return u.do(http.MethodPost, appPath(s, a.ID, "/actions"), ClientAction{
		Name: "add_note", SurfaceID: SurfaceID(a.ID), SourceComponentID: "add", Context: ctx})
}

func (w *world) fetch(u *user, s access.Space, a App, id string, thumb bool) *httptest.ResponseRecorder {
	suffix := ""
	if thumb {
		suffix = "/thumbnail"
	}
	return u.do(http.MethodGet, appPath(s, a.ID, "/attachments/"+id+suffix), nil)
}

// recordWith inserts a record holding the attachments ids in photos, as u, and returns its id.
func (w *world) recordWith(u *user, s access.Space, a App, ids ...string) int64 {
	w.e.t.Helper()
	list := make([]any, len(ids))
	for i, id := range ids {
		list[i] = id
	}
	expect(w.e.t, w.insert(u, s, a, map[string]any{"photos": list}), http.StatusOK)
	var id int64
	if err := w.e.db.QueryRow(`SELECT max(id) FROM app_rows WHERE app_id = ?`, a.ID).Scan(&id); err != nil {
		w.e.t.Fatal(err)
	}
	return id
}

func (w *world) rowCount(a App) int {
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM app_rows WHERE app_id = ?`, a.ID).Scan(&n); err != nil {
		w.e.t.Fatal(err)
	}
	return n
}

func (w *world) attachedTo(id string) (row *int64) {
	w.e.t.Helper()
	var n *int64
	if err := w.e.db.QueryRow(`SELECT row_id FROM app_attachments WHERE id = ?`, id).Scan(&n); err != nil {
		w.e.t.Fatal(err)
	}
	return n
}

func (w *world) files(s access.Space, a App) []string {
	dir := filepath.Join(s.Dir(w.e.dataDir), filesDir, fmt.Sprint(a.ID))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func expectCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	expect(t, rec, status)
	if got := decode[struct{ Code string }](t, rec).Code; got != code {
		t.Fatalf("code = %q, want %q; body %s", got, code, rec.Body)
	}
}

func TestUploadThenAttachThenRead(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	picture := testPNG(t, 640, 480)

	id := w.upload(w.alice, s, a, "photos", picture, 1000)
	// Before it is in a record the uploader reads it, to see what they are about to submit.
	rec := w.fetch(w.alice, s, a, id, false)
	expect(t, rec, http.StatusOK)
	if !bytes.Equal(rec.Body.Bytes(), picture) {
		t.Fatal("the picture came back changed")
	}
	h := rec.Header()
	if h.Get("Content-Type") != "image/png" || h.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(h.Get("Content-Disposition"), `inline; filename="photo-`) ||
		!strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") ||
		!strings.Contains(h.Get("Cache-Control"), "private") || h.Get("ETag") == "" {
		t.Fatalf("headers = %v", h)
	}
	thumb := w.fetch(w.alice, s, a, id, true)
	expect(t, thumb, http.StatusOK)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(thumb.Body.Bytes()))
	if err != nil || format != "jpeg" || cfg.Width != 320 || cfg.Height != 240 {
		t.Fatalf("thumbnail %v %s %v", cfg, format, err)
	}
	if thumb.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumbnail type %q", thumb.Header().Get("Content-Type"))
	}
	// A conditional request is answered without the bytes.
	r := httptest.NewRequest(http.MethodGet, appPath(s, a.ID, "/attachments/"+id), nil)
	r.Header.Set("If-None-Match", h.Get("ETag"))
	r.AddCookie(w.alice.cookie)
	cond := httptest.NewRecorder()
	w.e.h.ServeHTTP(cond, r)
	if cond.Code != http.StatusNotModified {
		t.Fatalf("conditional get = %d", cond.Code)
	}

	// Insert a record with it: the screen's data carries the id, and the record is attached.
	rec = w.insert(w.alice, s, a, map[string]any{"photos": []any{id}})
	expect(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), id) {
		t.Fatalf("the answer doesn't list the photo: %s", rec.Body)
	}
	if w.attachedTo(id) == nil {
		t.Fatal("the attachment isn't attached to the record")
	}
	// The surface tells the screen the field's limits.
	surface := w.alice.do(http.MethodGet, appPath(s, a.ID, "/surface"), nil).Body.String()
	if !strings.Contains(surface, `"_files":{"photos":{"max_bytes":200000,"max_files":2},"receipt":{"max_bytes":8388608,"max_files":1}}`) {
		t.Fatalf("surface lacks the limits: %s", surface)
	}
	expect(t, w.fetch(w.alice, s, a, id, true), http.StatusOK)

	// The file is in the space's managed storage, not the database.
	if got := w.files(s, a); len(got) != 2 {
		t.Fatalf("files = %v", got)
	}
	var blobs int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM pragma_table_info('app_attachments') WHERE type = 'BLOB'`).Scan(&blobs); err != nil || blobs != 0 {
		t.Fatalf("blob columns: %d %v", blobs, err)
	}

	// The agent reads metadata, never bytes.
	items, err := w.e.s.ListRows(context.Background(), Actor{UserID: w.alice.id, Agent: true}, s.ID, a.ID)
	if err != nil || len(items) != 1 {
		t.Fatalf("ListRows = %v %v", items, err)
	}
	got, _ := items[0]["photos"].([]Described)
	if len(got) != 1 || got[0].ID != id || got[0].ContentType != "image/png" || got[0].Width != 640 || got[0].Height != 480 || got[0].Size != int64(len(picture)) {
		t.Fatalf("agent sees %+v", items[0]["photos"])
	}
}

// A chunk whose answer was lost is sent again, and is refused with where the server is: no byte
// is written twice, and the finished file is exactly the picture, once.
func TestInterruptedUploadResumesWithoutDuplicates(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	picture := testPNG(t, 500, 400)
	third := len(picture)/3 + 1

	id := w.start(w.alice, s, a, "photos", len(picture))
	expect(t, w.chunk(w.alice, s, a, id, 0, picture[:third]), http.StatusOK)
	// The same chunk again (the client never heard it was applied): refused, and the answer says
	// where to go on.
	rec := w.chunk(w.alice, s, a, id, 0, picture[:third])
	expectCode(t, rec, http.StatusConflict, "app_upload_offset")
	if decode[struct{ Params map[string]int64 }](t, rec).Params["offset"] != int64(third) {
		t.Fatalf("offset in %s", rec.Body)
	}
	// A chunk from beyond the offset is refused too.
	expectCode(t, w.chunk(w.alice, s, a, id, third+10, picture[third+10:]), http.StatusConflict, "app_upload_offset")
	// Asking where the server is gives the same.
	rec = w.alice.do(http.MethodGet, appPath(s, a.ID, "/uploads/"+id), nil)
	expect(t, rec, http.StatusOK)
	if got := decode[Attachment](t, rec); got.Received != int64(third) || got.Status != "uploading" {
		t.Fatalf("state = %+v", got)
	}
	// Finalizing early says where the server is.
	expectCode(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+id+"/finalize"), map[string]any{}), http.StatusConflict, "app_upload_offset")

	// The server restarts: a new service over the same store and files goes on from the offset.
	restarted := New(w.e.db, w.e.s.access).WithFiles(w.e.dataDir)
	if _, err := restarted.putChunk(context.Background(), s, Actor{UserID: w.alice.id}, a.ID, id, int64(third), picture[third:2*third]); err != nil {
		t.Fatal(err)
	}

	// A chunk that was written and never recorded (the server stopped in between) leaves bytes
	// past the offset: the rest is written over them, and what they are is cut off.
	part := filepath.Join(s.Dir(w.e.dataDir), filesDir, fmt.Sprint(a.ID), id+".part")
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("stray bytes that were never recorded")
	_ = f.Close()
	expect(t, w.chunk(w.alice, s, a, id, 2*third, picture[2*third:]), http.StatusOK)
	rec = w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+id+"/finalize"), map[string]any{})
	expect(t, rec, http.StatusOK)
	// Finalizing again says the same.
	again := w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+id+"/finalize"), map[string]any{})
	expect(t, again, http.StatusOK)
	if again.Body.String() != rec.Body.String() {
		t.Fatalf("finalize twice: %s then %s", rec.Body, again.Body)
	}

	// Exactly the picture, and only its two files.
	got := w.fetch(w.alice, s, a, id, false)
	expect(t, got, http.StatusOK)
	if !bytes.Equal(got.Body.Bytes(), picture) {
		t.Fatalf("the file has %d bytes, the picture %d", got.Body.Len(), len(picture))
	}
	if files := w.files(s, a); len(files) != 2 {
		t.Fatalf("files = %v", files)
	}
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM app_attachments`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("attachments: %d %v", n, err)
	}
	// A chunk after finalizing is refused.
	expectCode(t, w.chunk(w.alice, s, a, id, len(picture), []byte("x")), http.StatusConflict, "app_upload_state")
}

func TestUploadChecksTypeSizeAndBounds(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	post := func(body map[string]any) *httptest.ResponseRecorder {
		return w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads"), body)
	}
	// Declared size: at least a byte, at most the field's.
	expectCode(t, post(map[string]any{"field": "photos", "size": 0}), http.StatusBadRequest, "app_upload_size")
	expectCode(t, post(map[string]any{"field": "photos", "size": 200001}), http.StatusBadRequest, "app_upload_size")
	expect(t, post(map[string]any{"field": "photos", "size": 200000}), http.StatusCreated)
	// Only an attachment field of the app takes one.
	expect(t, post(map[string]any{"field": "title", "size": 10}), http.StatusBadRequest)
	expect(t, post(map[string]any{"field": "nothing", "size": 10}), http.StatusBadRequest)

	// Not a picture, whatever it is called: refused at finalize, and nothing is left.
	for name, data := range map[string][]byte{
		"text":            []byte("hello, this is not a picture"),
		"html":            []byte("<html><script>alert(1)</script></html>"),
		"svg":             []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"truncated":       testPNG(t, 50, 50)[:40],
		"pdf":             []byte("%PDF-1.4 not a picture"),
		"too many pixels": pngClaiming(t, 8000, 8000),
		"no pixels":       pngClaiming(t, 0, 0),
	} {
		id := w.start(w.alice, s, a, "photos", len(data))
		w.send(w.alice, s, a, id, data, 100)
		rec := w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+id+"/finalize"), map[string]any{})
		if rec.Code != http.StatusBadRequest || decode[struct{ Code string }](t, rec).Code != "app_attachment_type" {
			t.Fatalf("%s: finalize = %d %s", name, rec.Code, rec.Body)
		}
		var n int
		if err := w.e.db.QueryRow(`SELECT count(*) FROM app_attachments WHERE id = ?`, id).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: the upload is still there", name)
		}
	}
	if files := w.files(s, a); len(files) != 0 {
		t.Fatalf("files left: %v", files)
	}

	// JPEG, PNG and GIF are accepted; the type is the bytes'.
	for want, data := range map[string][]byte{"image/jpeg": testJPEG(t, 30, 20), "image/png": testPNG(t, 30, 20), "image/gif": testGIF(t)} {
		id := w.upload(w.alice, s, a, "photos", data, 4096)
		if got := w.fetch(w.alice, s, a, id, false).Header().Get("Content-Type"); got != want {
			t.Fatalf("type %q, want %q", got, want)
		}
	}

	// A chunk is bounded: over the maximum, or past the declared size.
	id := w.start(w.alice, s, a, "photos", 100)
	expectCode(t, w.chunk(w.alice, s, a, id, 0, make([]byte, 101)), http.StatusRequestEntityTooLarge, "app_upload_chunk")
	expectCode(t, w.chunk(w.alice, s, a, id, 0, make([]byte, MaxChunk+1)), http.StatusRequestEntityTooLarge, "app_upload_chunk")
	expect(t, w.chunk(w.alice, s, a, id, 0, nil), http.StatusBadRequest)
	expect(t, w.alice.raw(http.MethodPut, appPath(s, a.ID, "/uploads/"+id+"?offset=-1"), []byte("x")), http.StatusBadRequest)
	expect(t, w.alice.raw(http.MethodPut, appPath(s, a.ID, "/uploads/"+id), []byte("x")), http.StatusBadRequest)

	// Too many waiting at once.
	var last *httptest.ResponseRecorder
	for range MaxPendingAttachments + 1 {
		if last = post(map[string]any{"field": "receipt", "size": 10}); last.Code != http.StatusCreated {
			break
		}
	}
	expectCode(t, last, http.StatusConflict, "app_uploads_pending")
}

func TestUploadRoutesAreProtected(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.createPhotos(w.alice, s, "Photos")
	id := w.start(w.alice, s, a, "photos", 10)

	// No session.
	anon := &user{e: w.e}
	expect(t, anon.do(http.MethodPost, appPath(s, a.ID, "/uploads"), map[string]any{"field": "photos", "size": 1}), http.StatusUnauthorized)
	expect(t, anon.raw(http.MethodPut, appPath(s, a.ID, "/uploads/"+id+"?offset=0"), []byte("x")), http.StatusUnauthorized)
	expect(t, anon.do(http.MethodGet, appPath(s, a.ID, "/attachments/"+id), nil), http.StatusUnauthorized)

	// The CSRF guard: a chunk is bytes, and only bytes; a cross-site one is refused; and a
	// state-changing request is never a GET.
	put := func(contentType, site string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, appPath(s, a.ID, "/uploads/"+id+"?offset=0"), strings.NewReader("x"))
		r.Header.Set("Content-Type", contentType)
		r.Header.Set("Sec-Fetch-Site", site)
		r.AddCookie(w.alice.cookie)
		rec := httptest.NewRecorder()
		w.e.h.ServeHTTP(rec, r)
		return rec
	}
	expect(t, put("application/octet-stream", "cross-site"), http.StatusForbidden)
	expect(t, put("text/plain", "same-origin"), http.StatusUnsupportedMediaType)
	expect(t, put("multipart/form-data; boundary=x", "same-origin"), http.StatusUnsupportedMediaType)
	// JSON passes the guard (as on any route) and the route itself wants bytes.
	expect(t, put("application/json", "same-origin"), http.StatusUnsupportedMediaType)
	expect(t, put("application/octet-stream", "same-origin"), http.StatusOK)
	// Creating an upload takes JSON, not bytes.
	expect(t, w.alice.raw(http.MethodPost, appPath(s, a.ID, "/uploads"), []byte(`{"field":"photos","size":1}`)), http.StatusUnsupportedMediaType)
	// A GET changes nothing: it can't create, write or finalize.
	for _, p := range []string{"/uploads", "/uploads/" + id + "/finalize"} {
		if rec := w.alice.do(http.MethodGet, appPath(s, a.ID, p), nil); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d", p, rec.Code)
		}
	}
	var received int64
	_ = w.e.db.QueryRow(`SELECT received FROM app_attachments WHERE id = ?`, id).Scan(&received)
	if received != 1 {
		t.Fatalf("received %d, want only the one accepted byte", received)
	}

	// A viewer reads the space and uploads nothing.
	expectForbidden0(t, w.bob.do(http.MethodPost, appPath(s, a.ID, "/uploads"), map[string]any{"field": "photos", "size": 1}))
	expectForbidden0(t, w.bob.raw(http.MethodPut, appPath(s, a.ID, "/uploads/"+id+"?offset=1"), []byte("x")))
}

func expectForbidden0(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 403 or 404; %s", rec.Code, rec.Body)
	}
}

// Only the uploader may continue, ask about, finalize or drop an upload; to everyone else, in the
// space or not, in this app or another, it isn't there.
func TestUploadsBelongToTheirUploader(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.createPhotos(w.alice, s, "Photos")
	other := w.createPhotos(w.alice, s, "Other photos")
	picture := testPNG(t, 40, 30)
	id := w.start(w.alice, s, a, "photos", len(picture))
	w.send(w.alice, s, a, id, picture[:50], 50)

	probe := func(who string, u *user, sp access.Space, app App) {
		t.Helper()
		for name, rec := range map[string]*httptest.ResponseRecorder{
			"state":    u.do(http.MethodGet, appPath(sp, app.ID, "/uploads/"+id), nil),
			"chunk":    u.raw(http.MethodPut, appPath(sp, app.ID, "/uploads/"+id+"?offset=50"), picture[50:]),
			"finalize": u.do(http.MethodPost, appPath(sp, app.ID, "/uploads/"+id+"/finalize"), map[string]any{}),
			"abandon":  u.do(http.MethodDelete, appPath(sp, app.ID, "/uploads/"+id), nil),
		} {
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
				t.Fatalf("%s, %s: %d %s", who, name, rec.Code, rec.Body)
			}
		}
	}
	// The owner is an editor of the space too, and still not the uploader.
	probe("another editor", w.owner, s, a)
	probe("a viewer", w.bob, s, a)
	probe("a member of no shared space", w.mallory, s, a)
	probe("another workspace's admin, through the space", w.stranger, in(w.other, s), a)
	probe("the uploader, through another app", w.alice, s, other)
	probe("the uploader, through another space", w.alice, w.alicePrivate, a)
	// Nothing of it changed.
	var received int64
	var status string
	if err := w.e.db.QueryRow(`SELECT received, status FROM app_attachments WHERE id = ?`, id).Scan(&received, &status); err != nil || received != 50 || status != "uploading" {
		t.Fatalf("upload = %d %s %v", received, status, err)
	}
	// The uploader goes on.
	expect(t, w.chunk(w.alice, s, a, id, 50, picture[50:]), http.StatusOK)
	_ = w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+id+"/finalize"), map[string]any{})
	expect(t, w.fetch(w.alice, s, a, id, false), http.StatusOK)
}

func TestRecordsNeverHoldInaccessibleAttachments(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.createPhotos(w.alice, s, "Photos")
	other := w.createPhotos(w.alice, s, "Other photos")
	picture := testPNG(t, 40, 30)

	mine := w.upload(w.alice, s, a, "photos", picture, 4096)
	theirs := w.upload(w.owner, s, a, "photos", picture, 4096) // the owner's: not alice's to attach
	unfinished := w.start(w.alice, s, a, "photos", len(picture))
	inOther := w.upload(w.alice, s, other, "photos", picture, 4096)
	wrongField := w.upload(w.alice, s, a, "receipt", picture, 4096)
	taken := w.upload(w.alice, s, a, "photos", picture, 4096)
	w.recordWith(w.alice, s, a, taken)

	refused := func(why string, fields map[string]any) {
		t.Helper()
		before := w.rowCount(a)
		rec := w.insert(w.alice, s, a, fields)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d %s", why, rec.Code, rec.Body)
		}
		if code := decode[struct{ Code string }](t, rec).Code; code != "app_attachment_bad" && code != "app_field_files" {
			t.Fatalf("%s: code %s", why, code)
		}
		if w.rowCount(a) != before {
			t.Fatalf("%s: a record was written", why)
		}
	}
	refused("another user's", map[string]any{"photos": []any{theirs}})
	refused("not finalized", map[string]any{"photos": []any{unfinished}})
	refused("another app's", map[string]any{"photos": []any{inOther}})
	refused("another field's", map[string]any{"photos": []any{wrongField}})
	refused("already in a record", map[string]any{"photos": []any{taken}})
	refused("one that doesn't exist", map[string]any{"photos": []any{strings.Repeat("ab", 16)}})
	refused("not an id", map[string]any{"photos": []any{"../../etc/passwd"}})
	refused("not a list", map[string]any{"photos": 7})
	refused("the same twice", map[string]any{"photos": []any{mine, mine}})
	refused("too many", map[string]any{"photos": []any{mine, w.upload(w.alice, s, a, "photos", picture, 4096), w.upload(w.alice, s, a, "photos", picture, 4096)}})
	// The same refusal, whatever the reason: someone else's id isn't told from a missing one.
	a1 := decode[struct{ Error string }](t, w.insert(w.alice, s, a, map[string]any{"photos": []any{theirs}})).Error
	a2 := decode[struct{ Error string }](t, w.insert(w.alice, s, a, map[string]any{"photos": []any{strings.Repeat("cd", 16)}})).Error
	if a1 != a2 {
		t.Fatalf("%q vs %q", a1, a2)
	}

	// A refused change attaches nothing, valid ones among its ids included.
	refused("one good, one bad", map[string]any{"photos": []any{mine, theirs}})
	if w.attachedTo(mine) != nil {
		t.Fatal("a refused record kept an attachment")
	}
	// And it is still usable.
	expect(t, w.insert(w.alice, s, a, map[string]any{"photos": []any{mine}}), http.StatusOK)
	if w.attachedTo(mine) == nil {
		t.Fatal("not attached")
	}

	// An update obeys the same checks, and a record keeps what it had.
	row := w.recordWith(w.alice, s, a, w.upload(w.alice, s, a, "photos", picture, 4096))
	update := func(fields map[string]any) *httptest.ResponseRecorder {
		ctx := map[string]any{"id": row}
		for k, v := range fields {
			ctx[k] = v
		}
		return w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), ClientAction{Name: "edit_note", SurfaceID: SurfaceID(a.ID), Context: ctx})
	}
	expect(t, update(map[string]any{"photos": []any{theirs}}), http.StatusUnprocessableEntity)
	expect(t, update(map[string]any{"photos": []any{taken}}), http.StatusUnprocessableEntity)
	expect(t, update(map[string]any{"title": "renamed"}), http.StatusOK)
	var data string
	_ = w.e.db.QueryRow(`SELECT data FROM app_rows WHERE id = ?`, row).Scan(&data)
	if !strings.Contains(data, `"photos":["`) {
		t.Fatalf("the record lost its photos: %s", data)
	}
}

// An action from an older release is held to the same checks as one from the live release.
func TestOlderReleaseActionsCheckAttachments(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.createPhotos(w.alice, s, "Photos")
	first := a.Live.Number
	// Publish a second release that adds a field.
	var def map[string]any
	if err := jsonUnmarshal(photoDefinition, &def); err != nil {
		t.Fatal(err)
	}
	fields := def["collection"].(map[string]any)["fields"].([]any)
	def["collection"].(map[string]any)["fields"] = append(fields, map[string]any{"name": "note", "type": "string"})
	cur := decode[App](t, w.alice.do(http.MethodGet, appPath(s, a.ID, ""), nil))
	expect(t, w.alice.do(http.MethodPatch, appPath(s, a.ID, ""), map[string]any{"version": cur.Version, "definition": def}), http.StatusOK)
	cur = decode[App](t, w.alice.do(http.MethodGet, appPath(s, a.ID, ""), nil))
	cur = w.publish(w.alice, s, cur)
	if cur.Live.Number == first {
		t.Fatal("not a new release")
	}

	picture := testPNG(t, 40, 30)
	theirs := w.upload(w.owner, s, a, "photos", picture, 4096)
	mine := w.upload(w.alice, s, a, "photos", picture, 4096)
	old := func(id string) *httptest.ResponseRecorder {
		release := first
		return w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), ClientAction{
			Name: "add_note", SurfaceID: SurfaceID(a.ID), Release: &release, Context: map[string]any{"title": "x", "photos": []any{id}}})
	}
	before := w.rowCount(a)
	expect(t, old(theirs), http.StatusUnprocessableEntity)
	if w.rowCount(a) != before || w.attachedTo(theirs) != nil {
		t.Fatal("an older release's action took an attachment that isn't the caller's")
	}
	expect(t, old(mine), http.StatusOK)
	if w.attachedTo(mine) == nil {
		t.Fatal("an older release's action didn't attach the caller's own")
	}
}

func TestReplacingAndDeletingKeepsFiles(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.createPhotos(w.alice, s, "Photos")
	picture := testPNG(t, 40, 30)
	first := w.upload(w.alice, s, a, "photos", picture, 4096)
	second := w.upload(w.alice, s, a, "photos", picture, 4096)
	row := w.recordWith(w.alice, s, a, first)

	// Replace the photo: the new one is claimed, and the old one stays with the record, which its
	// history refers to.
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), ClientAction{
		Name: "edit_note", SurfaceID: SurfaceID(a.ID), Context: map[string]any{"id": row, "photos": []any{second}}}), http.StatusOK)
	if w.attachedTo(first) == nil || w.attachedTo(second) == nil {
		t.Fatal("an attachment came off its record")
	}
	expect(t, w.fetch(w.bob, s, a, first, false), http.StatusOK)
	// It can't be put on another record, though.
	expect(t, w.insert(w.alice, s, a, map[string]any{"photos": []any{first}}), http.StatusUnprocessableEntity)
	if err := w.e.s.Sweep(context.Background(), time.Now().Add(100*time.Hour)); err != nil {
		t.Fatal(err)
	}
	expect(t, w.fetch(w.bob, s, a, first, true), http.StatusOK)
}

// What a caller may read of an attachment is what they may read of its record.
func TestAttachmentsAreReadAsTheirRecordsAre(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.createPhotos(w.alice, s, "Photos")
	// owner may submit and read only their own records; alice does everything.
	w.mustSetRoles(w.alice, s, a, grant(w.owner.id, access.AppUser), grant(w.alice.id, access.AppRoles...), fallback(access.AppReader))
	picture := testPNG(t, 40, 30)

	alices := w.upload(w.alice, s, a, "photos", picture, 4096)
	w.recordWith(w.alice, s, a, alices)
	owners := w.upload(w.owner, s, a, "photos", picture, 4096)
	pending := w.upload(w.alice, s, a, "photos", picture, 4096) // never put in a record
	ownersRow := w.recordWith(w.owner, s, a, owners)

	for _, thumb := range []bool{false, true} {
		expect(t, w.fetch(w.alice, s, a, alices, thumb), http.StatusOK)
		expect(t, w.fetch(w.alice, s, a, owners, thumb), http.StatusOK) // reads all
		expect(t, w.fetch(w.owner, s, a, owners, thumb), http.StatusOK) // their own
		// Their role reads only their own records: alice's is as if it didn't exist.
		own := w.fetch(w.owner, s, a, alices, thumb)
		expect(t, own, http.StatusNotFound)
		missing := w.fetch(w.owner, s, a, strings.Repeat("ef", 16), thumb)
		expect(t, missing, http.StatusNotFound)
		if own.Body.String() != missing.Body.String() {
			t.Fatalf("unreadable %q, missing %q", own.Body, missing.Body)
		}
		// Not in the space, or in no workspace: the same.
		expect(t, w.fetch(w.mallory, s, a, alices, thumb), http.StatusNotFound)
		expect(t, w.fetch(w.stranger, in(w.other, s), a, alices, thumb), http.StatusNotFound)
		// Another space's path doesn't reach it.
		expect(t, w.fetch(w.alice, w.alicePrivate, a, alices, thumb), http.StatusNotFound)
		// A viewer reads every record.
		expect(t, w.fetch(w.bob, s, a, alices, thumb), http.StatusOK)
		// Before it is in a record only its uploader reads it.
		expect(t, w.fetch(w.alice, s, a, pending, thumb), http.StatusOK)
		expect(t, w.fetch(w.bob, s, a, pending, thumb), http.StatusNotFound)
		expect(t, w.fetch(w.owner, s, a, pending, thumb), http.StatusNotFound)
	}
	// An unfinished upload is no attachment to anyone.
	id := w.start(w.alice, s, a, "photos", 10)
	expect(t, w.fetch(w.alice, s, a, id, false), http.StatusNotFound)
	// Nothing that isn't an id gets to a file.
	for _, bad := range []string{"..", "..%2F..%2Fetc%2Fpasswd", strings.Repeat("g", 32), strings.Repeat("a", 31), "x"} {
		if rec := w.alice.do(http.MethodGet, appPath(s, a.ID, "/attachments/"+bad), nil); rec.Code != http.StatusNotFound && rec.Code != http.StatusTemporaryRedirect && rec.Code != http.StatusMovedPermanently {
			t.Fatalf("%q: %d", bad, rec.Code)
		}
	}
	_ = ownersRow
}

func TestADeletedRecordsAttachmentsAreHidden(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.createPhotos(w.alice, s, "Photos")
	picture := testPNG(t, 40, 30)
	id := w.upload(w.alice, s, a, "photos", picture, 4096)
	row := w.recordWith(w.alice, s, a, id)
	expect(t, w.fetch(w.bob, s, a, id, false), http.StatusOK)

	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/actions"), ClientAction{
		Name: "remove_note", SurfaceID: SurfaceID(a.ID), Context: map[string]any{"id": row}}), http.StatusOK)
	for _, u := range []*user{w.alice, w.bob, w.owner} {
		for _, thumb := range []bool{false, true} {
			expect(t, w.fetch(u, s, a, id, thumb), http.StatusNotFound)
		}
	}
	// The files are kept, and come back with the record.
	if got := w.files(s, a); len(got) != 2 {
		t.Fatalf("files = %v", got)
	}
	expect(t, w.alice.do(http.MethodPost, appPath(s, a.ID, fmt.Sprintf("/records/%d/restore", row)), map[string]any{}), http.StatusOK)
	expect(t, w.fetch(w.bob, s, a, id, false), http.StatusOK)
}

func TestExpiredAttachmentsAreSwept(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.createPhotos(w.alice, s, "Photos")
	picture := testPNG(t, 40, 30)
	kept := w.upload(w.alice, s, a, "photos", picture, 4096)
	w.recordWith(w.alice, s, a, kept)
	orphan := w.upload(w.alice, s, a, "photos", picture, 4096)
	abandoned := w.start(w.alice, s, a, "photos", 100)
	w.send(w.alice, s, a, abandoned, make([]byte, 40), 40)

	// Nothing is old yet.
	if err := w.e.s.Sweep(context.Background(), time.Now().Add(-AttachmentTTL)); err != nil {
		t.Fatal(err)
	}
	expect(t, w.fetch(w.alice, s, a, orphan, false), http.StatusOK)

	// A stray file no row knows, and a directory of an app that is gone.
	dir := filepath.Join(s.Dir(w.e.dataDir), filesDir, fmt.Sprint(a.ID))
	stray := filepath.Join(dir, strings.Repeat("0", 32)+".thumb")
	gone := filepath.Join(s.Dir(w.e.dataDir), filesDir, "99999")
	if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(gone, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gone, strings.Repeat("1", 32)), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.e.s.Sweep(context.Background(), time.Now().Add(-AttachmentTTL)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("a recent file was removed: %v", err)
	}

	// A day on.
	if err := w.e.s.Sweep(context.Background(), time.Now().Add(AttachmentTTL+time.Hour)); err != nil {
		t.Fatal(err)
	}
	expect(t, w.fetch(w.alice, s, a, orphan, false), http.StatusNotFound)
	expect(t, w.fetch(w.alice, s, a, kept, false), http.StatusOK) // in a record: never swept
	for _, id := range []string{orphan, abandoned} {
		var n int
		if err := w.e.db.QueryRow(`SELECT count(*) FROM app_attachments WHERE id = ?`, id).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: still a row", id)
		}
	}
	if got := w.files(s, a); len(got) != 2 {
		t.Fatalf("files after the sweep: %v (want the kept photo and its thumbnail)", got)
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatalf("the directory of an app that is gone is still there: %v", err)
	}

	// Starting an upload clears what has expired for its app, without a timer.
	old := w.upload(w.alice, s, a, "photos", picture, 4096)
	if _, err := w.e.db.Exec(`UPDATE app_attachments SET updated_at = '2000-01-01T00:00:00.000Z' WHERE id = ?`, old); err != nil {
		t.Fatal(err)
	}
	w.start(w.alice, s, a, "photos", 10)
	var n int
	if err := w.e.db.QueryRow(`SELECT count(*) FROM app_attachments WHERE id = ?`, old).Scan(&n); err != nil || n != 0 {
		t.Fatal("an expired attachment survived a new upload")
	}
}

func TestAbandoningAnUpload(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.createPhotos(w.alice, s, "Photos")
	picture := testPNG(t, 40, 30)
	id := w.upload(w.alice, s, a, "photos", picture, 4096)
	expect(t, w.alice.do(http.MethodDelete, appPath(s, a.ID, "/uploads/"+id), nil), http.StatusNoContent)
	expect(t, w.fetch(w.alice, s, a, id, false), http.StatusNotFound)
	if got := w.files(s, a); len(got) != 0 {
		t.Fatalf("files = %v", got)
	}
	// One in a record stays.
	kept := w.upload(w.alice, s, a, "photos", picture, 4096)
	w.recordWith(w.alice, s, a, kept)
	expectCode(t, w.alice.do(http.MethodDelete, appPath(s, a.ID, "/uploads/"+kept), nil), http.StatusConflict, "app_upload_state")
}

// Moving an app to another space keeps its photos readable, wherever the move got to, and brings
// the files along.
func TestMovingAnAppMovesItsFiles(t *testing.T) {
	w := newWorld(t)
	from, to := w.alicePrivate, w.shared
	a := w.createPhotos(w.alice, from, "Photos")
	picture := testPNG(t, 80, 60)
	id := w.upload(w.alice, from, a, "photos", picture, 4096)
	pending := w.upload(w.alice, from, a, "photos", picture, 4096)
	w.recordWith(w.alice, from, a, id)
	if len(w.files(from, a)) != 4 {
		t.Fatalf("files before: %v", w.files(from, a))
	}

	// The move can't copy the files (something is in the way at the new space): it still succeeds,
	// and the photos are read from where they are.
	blocker := filepath.Join(to.Dir(w.e.dataDir), filesDir)
	if err := os.MkdirAll(filepath.Dir(blocker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocker, []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	cur := decode[App](t, w.alice.do(http.MethodGet, appPath(from, a.ID, ""), nil))
	expect(t, w.alice.do(http.MethodPost, appPath(from, a.ID, "/move"), map[string]any{"version": cur.Version, "space_id": to.ID}), http.StatusOK)
	expect(t, w.fetch(w.alice, to, a, id, false), http.StatusOK)
	expect(t, w.fetch(w.alice, to, a, id, true), http.StatusOK)
	expect(t, w.fetch(w.alice, to, a, pending, false), http.StatusOK)
	expect(t, w.fetch(w.alice, from, a, id, false), http.StatusNotFound) // the old space no longer has the app
	// The readers of the new space read it; nobody of the old one does by that.
	expect(t, w.fetch(w.bob, to, a, id, false), http.StatusOK)
	expect(t, w.fetch(w.bob, from, a, id, false), http.StatusNotFound)
	// The way is cleared and the sweep goes on: the files are in the new space, and gone from the old.
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	// A new photo goes to the new space, and a record in the app takes it, whichever holds the others.
	more := w.upload(w.alice, to, a, "photos", picture, 4096)
	w.recordWith(w.alice, to, a, more)
	if err := w.e.s.Sweep(context.Background(), time.Now().Add(-AttachmentTTL)); err != nil {
		t.Fatal(err)
	}
	var space int64
	if err := w.e.db.QueryRow(`SELECT space_id FROM app_attachments WHERE id = ?`, id).Scan(&space); err != nil || space != to.ID {
		t.Fatalf("the attachment's space is %d, want %d (%v)", space, to.ID, err)
	}
	if got := w.files(to, a); len(got) != 6 {
		t.Fatalf("files in the new space: %v", got)
	}
	if got := w.files(from, a); len(got) != 0 {
		t.Fatalf("files left in the old space: %v", got)
	}
	got := w.fetch(w.alice, to, a, id, false)
	expect(t, got, http.StatusOK)
	if !bytes.Equal(got.Body.Bytes(), picture) {
		t.Fatal("the moved picture changed")
	}

	// A move with nothing in the way does it at once.
	b := w.createPhotos(w.alice, w.alicePrivate, "More photos")
	bid := w.upload(w.alice, w.alicePrivate, b, "photos", picture, 4096)
	w.recordWith(w.alice, w.alicePrivate, b, bid)
	cur = decode[App](t, w.alice.do(http.MethodGet, appPath(w.alicePrivate, b.ID, ""), nil))
	expect(t, w.alice.do(http.MethodPost, appPath(w.alicePrivate, b.ID, "/move"), map[string]any{"version": cur.Version, "space_id": to.ID}), http.StatusOK)
	if got := w.files(w.alicePrivate, b); len(got) != 0 {
		t.Fatalf("files left behind: %v", got)
	}
	if got := w.files(to, b); len(got) != 2 {
		t.Fatalf("files after the move: %v", got)
	}
	expect(t, w.fetch(w.alice, to, b, bid, false), http.StatusOK)
}

func TestAttachmentFieldDefinitions(t *testing.T) {
	check := func(def string) error {
		var d Definition
		if err := jsonUnmarshal(def, &d); err != nil {
			t.Fatal(err)
		}
		_, err := d.Check()
		return err
	}
	if err := check(photoDefinition); err != nil {
		t.Fatal(err)
	}
	var d Definition
	_ = jsonUnmarshal(photoDefinition, &d)
	checked, _ := d.Check()
	if !contains(checked.Requires, AttachmentCapability) || !contains(checked.Requires, "component.PhotoInput") || !contains(checked.Requires, "component.Photos") {
		t.Fatalf("requires = %v", checked.Requires)
	}
	if f, _ := checked.Collection.field("receipt"); f.MaxFiles != 1 || f.MaxBytes != DefaultMaxFileBytes {
		t.Fatalf("defaults = %+v", f)
	}
	// The capability is needed even when no screen draws a photo.
	bare := `{"components": [{"id": "root", "component": "Text", "text": "hi"}],
		"collection": {"fields": [{"name": "p", "type": "attachment"}]}, "actions": {"a": {"kind": "insert"}}}`
	var b Definition
	_ = jsonUnmarshal(bare, &b)
	if got, err := b.Check(); err != nil || !contains(got.Requires, AttachmentCapability) {
		t.Fatalf("bare: %v %v", got.Requires, err)
	}
	for name, mutate := range map[string]func(string) string{
		"version 1": func(s string) string {
			return strings.Replace(s, `"components": [`, `"definition_version": 1, "components": [`, 1)
		},
		"too many files":             func(s string) string { return strings.Replace(s, `"max_files": 2`, `"max_files": 11`, 1) },
		"too large files":            func(s string) string { return strings.Replace(s, `"max_bytes": 200000`, `"max_bytes": 17000000`, 1) },
		"negative bytes":             func(s string) string { return strings.Replace(s, `"max_bytes": 200000`, `"max_bytes": -1`, 1) },
		"a photo input for a string": func(s string) string { return strings.Replace(s, `"field": "photos"`, `"field": "title"`, 1) },
		"a photo input for nothing":  func(s string) string { return strings.Replace(s, `"field": "photos"`, `"field": "nope"`, 1) },
		"max_files on a string": func(s string) string {
			return strings.Replace(s, `{"name": "title", "type": "string"}`, `{"name": "title", "type": "string", "max_files": 2}`, 1)
		},
		"a rule on photos": func(s string) string {
			return strings.Replace(s, `{"name": "title", "type": "string"}`,
				`{"name": "title", "type": "string", "required_when": {"field": "photos", "op": "eq", "value": "x"}}`, 1)
		},
	} {
		if err := check(mutate(photoDefinition)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// An attachment field may itself be required, or conditionally.
	cond := strings.Replace(photoDefinition, `{"name": "receipt", "type": "attachment"}`,
		`{"name": "receipt", "type": "attachment", "required_when": {"field": "title", "op": "eq", "value": "refund"}}`, 1)
	if err := check(cond); err != nil {
		t.Fatal(err)
	}
}

func TestAttachmentValuesAreChecked(t *testing.T) {
	f := Field{Name: "p", Type: TypeAttachment, MaxFiles: 2}
	id := func(c string) string { return strings.Repeat(c, 32) }
	if v, ok, err := f.parse([]any{id("a"), id("b")}); err != nil || !ok || len(v.([]any)) != 2 {
		t.Fatalf("%v %v %v", v, ok, err)
	}
	if v, ok, err := f.parse(id("a")); err != nil || !ok || len(v.([]any)) != 1 {
		t.Fatalf("a single id: %v %v %v", v, ok, err)
	}
	for _, empty := range []any{nil, []any{}, "", "  "} {
		if _, ok, err := f.parse(empty); err != nil || ok {
			t.Fatalf("%v: present %v, %v", empty, ok, err)
		}
	}
	for name, bad := range map[string]any{
		"three":          []any{id("a"), id("b"), id("c")},
		"twice":          []any{id("a"), id("a")},
		"not hex":        []any{strings.Repeat("z", 32)},
		"short":          []any{"abc"},
		"upper":          []any{strings.Repeat("A", 32)},
		"a number":       []any{7},
		"an object":      map[string]any{"id": id("a")},
		"a number alone": 3.0,
	} {
		if _, _, err := f.parse(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPicturesAreInspectedAndThumbnailed(t *testing.T) {
	// A big picture is made small, and keeps its proportions.
	big := testJPEG(t, 2000, 1000)
	info, err := inspect(big)
	if err != nil || info.format != "jpeg" || info.width != 2000 || info.height != 1000 {
		t.Fatalf("inspect = %+v %v", info, err)
	}
	thumb, err := thumbnail(big, "jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(thumb)); err != nil || cfg.Width != 320 || cfg.Height != 160 {
		t.Fatalf("thumbnail = %+v %v", cfg, err)
	}
	// A small one isn't enlarged.
	small, err := thumbnail(testPNG(t, 30, 20), "png")
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _, _ := image.DecodeConfig(bytes.NewReader(small)); cfg.Width != 30 || cfg.Height != 20 {
		t.Fatalf("small thumbnail = %+v", cfg)
	}
	// A phone's photo says how it is to be turned: the picture is shown that way, and so is its
	// thumbnail.
	for o, swapped := range map[int]bool{1: false, 3: false, 6: true, 8: true} {
		jpg := withOrientation(testJPEG(t, 60, 40), o)
		info, err := inspect(jpg)
		if err != nil {
			t.Fatal(err)
		}
		th, err := thumbnail(jpg, "jpeg")
		if err != nil {
			t.Fatal(err)
		}
		cfg, _, _ := image.DecodeConfig(bytes.NewReader(th))
		wantW, wantH := 60, 40
		if swapped {
			wantW, wantH = 40, 60
		}
		if info.width != wantW || info.height != wantH || cfg.Width != wantW || cfg.Height != wantH {
			t.Fatalf("orientation %d: shown %dx%d, thumbnail %dx%d, want %dx%d", o, info.width, info.height, cfg.Width, cfg.Height, wantW, wantH)
		}
	}
	// A transparent PNG is laid on white.
	clear := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	_ = png.Encode(&buf, clear)
	th, err := thumbnail(buf.Bytes(), "png")
	if err != nil {
		t.Fatal(err)
	}
	img, _ := jpeg.Decode(bytes.NewReader(th))
	if r, g, b, _ := img.At(1, 1).RGBA(); r>>8 < 250 || g>>8 < 250 || b>>8 < 250 {
		t.Fatalf("transparent pixel = %d %d %d", r>>8, g>>8, b>>8)
	}
	// Garbage in the EXIF segment is no reason to fail.
	broken := append([]byte{0xff, 0xd8, 0xff, 0xe1, 0x00, 0x08, 'E', 'x', 'i', 'f', 0, 0}, testJPEG(t, 10, 10)[2:]...)
	if o := orientationOf(broken); o != 1 {
		t.Fatalf("orientation of garbage = %d", o)
	}
}

// A write whose context ends inside its transaction is rolled back before write returns: left to
// database/sql's own goroutine, the connection would still be busy, and a worker that closes the
// database at shutdown (or begins the next write) would meet a locked file.
func TestWriteCancelledInsideItsTransactionLeavesNoConnectionBusy(t *testing.T) {
	e := newEnv(t)
	for range 200 {
		ctx, cancel := context.WithCancel(context.Background())
		err := e.s.write(ctx, func(tx *sql.Tx) error {
			cancel()
			_, err := tx.ExecContext(ctx, `INSERT INTO users (email, name, password_hash) VALUES ('c@example.com', 'C', 'x')`)
			return err
		})
		if err == nil {
			t.Fatal("a write cancelled inside its transaction succeeded")
		}
		if inUse := e.db.Stats().InUse; inUse != 0 {
			t.Fatalf("%d connections still busy after write returned", inUse)
		}
	}
	// Nothing was written, and the next writer begins at once.
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM users WHERE email = 'c@example.com'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("users = %d, %v; want the cancelled insert rolled back", n, err)
	}
	if err := e.s.write(context.Background(), func(tx *sql.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// A sweep whose context has ended stops at once and removes nothing more.
func TestCancelledSweepRemovesNothing(t *testing.T) {
	w := newWorld(t)
	s := w.shared
	a := w.createPhotos(w.alice, s, "Photos")
	orphan := w.upload(w.alice, s, a, "photos", testPNG(t, 40, 30), 4096)
	stray := filepath.Join(s.Dir(w.e.dataDir), filesDir, fmt.Sprint(a.ID), strings.Repeat("0", 32)+".thumb")
	if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.e.s.Sweep(ctx, time.Now().Add(AttachmentTTL+time.Hour)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sweep = %v, want context.Canceled", err)
	}
	expect(t, w.fetch(w.alice, s, a, orphan, false), http.StatusOK)
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("a cancelled sweep removed a file: %v", err)
	}
	// The strays' scan itself checks between spaces, apps and files.
	if err := w.e.s.sweepStrays(ctx, time.Now().Add(AttachmentTTL+time.Hour)); !errors.Is(err, context.Canceled) {
		t.Fatalf("sweepStrays = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("a cancelled scan removed a file: %v", err)
	}
}
