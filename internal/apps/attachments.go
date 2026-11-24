package apps

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/quota"
)

// Photo attachments.
//
// A record holds no bytes: an attachment field's value is a list of attachment ids. The bytes are
// separate resources, kept in the managed storage of the space that holds the app
// (<space>/.taskto/app-files/<app>/<id>, with <id>.thumb beside it), reached only through the server's
// authenticated routes, never by an address anyone could hand to another. The database keeps
// each one's metadata (app_attachments, migration 0028).
//
// An attachment is made in three steps, by someone who may submit or change the app's records:
//
//  1. create an upload for a field of the app: the declared size is checked against the field's
//     limit, and the answer is the id;
//  2. send the bytes in chunks, each saying the offset it is at. The server's offset (what it has
//     received) is in every answer and in the upload's state, and a chunk at any other offset is
//     refused with it (ErrUploadOffset), so a chunk that was applied but whose answer was lost
//     is never applied twice: the client asks where the server is and goes on from there, and
//     the same bytes are never written twice. Only the uploader may add chunks, read the state,
//     finalize or abandon an upload;
//  3. finalize: the bytes must be all there, and be a JPEG, PNG or GIF by their own content (not
//     by anything the client said), within the pixel bound; the server makes the thumbnail, and
//     keeps the picture without its metadata (stripMetadata: where a photo was taken, the camera,
//     and the rest, left out without touching the pixels; only the orientation stays). The upload
//     becomes an attachment, "ready", whose size is that of the picture kept, and finalizing it
//     again says the same.
//
// A ready attachment is attached to a record by the insert or update that lists its id, in the
// same transaction as the record is written (claimAttachments): the attachment must be the
// caller's own, of this app and this field, finished, and not attached to anything yet, or the
// whole change is refused and nothing is written. Every route that writes records goes through
// it, an action of an older release included. Once attached, an attachment stays with its record
// for good: the record's history refers to it, so replacing it in the list, deleting the record
// or restoring it never deletes a file.
//
// Reading an attachment or its thumbnail takes reading the record it is attached to, as the
// caller's role in the app has it (all records, or only their own), and a deleted record's
// attachments are read by nobody until it is restored. Before it is attached, only its uploader
// reads it (to see what they are about to submit). Anyone else, for an attachment that doesn't
// exist or one they may not read, gets the same 404.
//
// What isn't attached to a record within AttachmentTTL of its last change is removed with its
// files: abandoned uploads, and photos taken and never submitted (Sweep). Files are removed
// after their rows, so a failure leaves a stray file, which Sweep finds (a file no row of its
// space knows) and removes too, as it does the files of an app that no longer exists.
//
// An app moved to another space of its workspace keeps working at once: each attachment's row
// names the space whose directory holds its bytes, reads follow it, and the server copies the
// files to the new space and updates the row (relocate), which Sweep retries if it was
// interrupted. No route deletes an app or a space today; when one does, the rows go with them
// (ON DELETE CASCADE) and Sweep removes the files.

// Bounds of attachments.
const (
	// ChunkSize is the size of chunk the server suggests; MaxChunk the largest it takes.
	ChunkSize = 1 << 20
	MaxChunk  = 2 << 20
	// MaxPendingAttachments bounds the uploads and finished photos of one person in one app that
	// aren't attached to a record yet.
	MaxPendingAttachments = 20
	// MaxAppFileBytes bounds the declared size of all of an app's attachments together.
	MaxAppFileBytes = 1 << 30
	// AttachmentTTL is how long an attachment that is attached to no record is kept after its last
	// change.
	AttachmentTTL = 24 * time.Hour
	// maxThumbnails bounds the pictures decoded at once, which bounds the memory they take.
	maxThumbnails = 2
)

// The states of an attachment.
const (
	statusUploading = "uploading"
	statusReady     = "ready"
)

// filesDir is the directory, inside a space's directory, that holds its apps' attachments, named for
// the apps' ids. It is in the server's managed storage (access.ManagedDir), which no document's name
// can reach.
const filesDir = access.ManagedDir + "/app-files"

var attachmentIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// errNotPicture is for bytes that aren't a picture the server accepts.
var errNotPicture = invalidAs(apierr.CodeAppAttachmentType, apierr.Params{"max_megapixels": maxPixels["jpeg"] / 1_000_000},
	"the file isn't a JPEG, PNG or GIF picture this server can read")

// errNotPNG is for a picture that isn't a PNG, offered for a signature.
var errNotPNG = invalidAs(apierr.CodeAppSignatureType, nil, "a signature is a PNG picture")

// statusError is an error with the status and code the API answers it with.
type statusError struct {
	status int
	code   apierr.Code
	msg    string
	params apierr.Params
}

func (e *statusError) Error() string { return e.msg }

var errNoFiles = &statusError{http.StatusServiceUnavailable, apierr.CodeAppFilesUnavailable, "this server keeps no files for apps", nil}

// Attachment is one attachment, or one upload on its way to be: what the API tells its uploader,
// and, once it is ready, those who read the record it is in.
type Attachment struct {
	ID    string `json:"id"`
	Field string `json:"field"`
	// Status is "uploading" or "ready".
	Status string `json:"status"`
	// While it is uploading, Size is what the upload declared and Received what the server has of
	// it so far: the offset the next chunk starts at. Once it is ready, both are the size of the
	// stored picture, which is the length it is served with: the received file without its metadata.
	Size     int64 `json:"size"`
	Received int64 `json:"received"`
	// ContentType, Width and Height are what the bytes turned out to be, once it is ready.
	ContentType string `json:"content_type,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	// Attached is whether a record holds it.
	Attached  bool   `json:"attached"`
	CreatedAt string `json:"created_at"`
	// ChunkSize and MaxChunk are in the answer that creates an upload.
	ChunkSize int `json:"chunk_size,omitempty"`
	MaxChunk  int `json:"max_chunk,omitempty"`
}

// NewUpload starts an upload: the attachment field it is for and the size of the file in bytes.
type NewUpload struct {
	Field string `json:"field"`
	Size  int64  `json:"size"`
}

// WithFiles gives the service the data directory whose spaces hold apps' attachments. Without
// one the attachment routes aren't served.
func (s *Service) WithFiles(dataDir string) *Service {
	s.dataDir = dataDir
	return s
}

// WithQuota gives the service the storage quotas, which an upload is checked against when it
// starts, at its declared size, and again when it is finalized, at what is kept.
func (s *Service) WithQuota(q *quota.Service) *Service {
	s.quota = q
	return s
}

// checkQuota refuses, with the quota's error, adding bytes that would take sp over a storage
// quota. q is the transaction the caller holds, so the uploads it has reserved count.
func (s *Service) checkQuota(ctx context.Context, q quota.Queryer, sp access.Space, adding int64) error {
	if s.quota == nil {
		return nil
	}
	return s.quota.Check(ctx, q, sp, adding)
}

// lock serializes what is done to one attachment's bytes, in this process (which is the data
// directory's only one). It returns what releases it.
func (s *Service) lock(id string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	mu := &s.locks[h.Sum32()%uint32(len(s.locks))]
	mu.Lock()
	return mu.Unlock
}

type fileLocks [64]sync.Mutex

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// filesRoot opens the directory of spaceID, which holds the files of attachments stored there.
func (s *Service) filesRoot(ctx context.Context, spaceID int64) (*os.Root, error) {
	if s.dataDir == "" {
		return nil, errNoFiles
	}
	var workspace int64
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_id FROM spaces WHERE id = ?`, spaceID).Scan(&workspace); err != nil {
		return nil, err
	}
	return access.OpenRoot(s.dataDir, access.Space{ID: spaceID, WorkspaceID: workspace})
}

// fileName is the name, inside a space's directory, of an attachment's file with suffix: "" for
// the picture, ".part" while it is received, ".thumb" for the thumbnail. The names are made of
// numbers and the id's hexadecimal digits only.
func fileName(appID int64, id, suffix string) string {
	return path.Join(filesDir, strconv.FormatInt(appID, 10), id+suffix)
}

// upload is an attachment as the store has it.
type upload struct {
	Attachment
	appID   int64
	spaceID int64
	owner   sql.NullInt64
	rowID   sql.NullInt64
}

const attachmentColumns = `id, app_id, space_id, field, owner, status, size, received, coalesce(content_type, ''),
	coalesce(width, 0), coalesce(height, 0), row_id, created_at`

func scanAttachment(row scanner) (upload, error) {
	var u upload
	if err := row.Scan(&u.ID, &u.appID, &u.spaceID, &u.Field, &u.owner, &u.Status, &u.Size, &u.Received, &u.ContentType,
		&u.Width, &u.Height, &u.rowID, &u.CreatedAt); err != nil {
		return upload{}, err
	}
	u.Attached = u.rowID.Valid
	return u, nil
}

// loadAttachment returns attachment id of appID, or ErrNotFound.
func loadAttachment(ctx context.Context, q queryer, appID int64, id string) (upload, error) {
	if !attachmentIDPattern.MatchString(id) {
		return upload{}, ErrNotFound
	}
	u, err := scanAttachment(q.QueryRowContext(ctx, `SELECT `+attachmentColumns+` FROM app_attachments WHERE id = ? AND app_id = ?`, id, appID))
	if errors.Is(err, sql.ErrNoRows) {
		return upload{}, ErrNotFound
	}
	return u, err
}

// uploader checks, inside a write transaction, that a may still write the space and may submit or
// change records of the app, as every step of an upload needs.
func (s *Service) uploader(ctx context.Context, a Actor, spaceID, appID int64) error {
	acc, err := s.recheckApp(ctx, a, spaceID, appID)
	if err != nil {
		return err
	}
	if !acc.Submit && acc.ModifyRecords == access.ScopeNone {
		return forbidden(string(access.CapSubmit))
	}
	return nil
}

// ownUpload loads attachment id for its uploader: anyone else's, or one that doesn't exist, is
// ErrNotFound.
func ownUpload(ctx context.Context, q queryer, a Actor, appID int64, id string) (upload, error) {
	u, err := loadAttachment(ctx, q, appID, id)
	if err != nil {
		return upload{}, err
	}
	if !u.owner.Valid || u.owner.Int64 != a.UserID {
		return upload{}, ErrNotFound
	}
	return u, nil
}

func offsetError(received int64) error {
	return &statusError{http.StatusConflict, apierr.CodeAppUploadOffset,
		fmt.Sprintf("the server has %d bytes of this upload: continue from there", received), apierr.Params{"offset": received}}
}

var errUploadState = &statusError{http.StatusConflict, apierr.CodeAppUploadState, "the upload isn't in the state this needs", nil}

// createUpload starts an upload for field of the live release's collection.
func (s *Service) createUpload(ctx context.Context, sp access.Space, a Actor, appID int64, in NewUpload) (Attachment, error) {
	if err := may(sp, access.Write); err != nil {
		return Attachment{}, err
	}
	if s.dataDir == "" {
		return Attachment{}, errNoFiles
	}
	id, err := newID()
	if err != nil {
		return Attachment{}, err
	}
	// What was abandoned is cleared as uploads come in, so a quiet server needs no timer.
	if err := s.sweep(ctx, time.Now().Add(-AttachmentTTL), appID); err != nil {
		slog.Warn("apps: removing expired attachments", "app", appID, "err", err)
	}
	var created Attachment
	err = s.write(ctx, func(tx *sql.Tx) error {
		if err := s.uploader(ctx, a, sp.ID, appID); err != nil {
			return err
		}
		ap, err := launchable(ctx, tx, sp.ID, appID)
		if err != nil {
			return err
		}
		if ap.liveDef == nil {
			return ErrNotPublished
		}
		f, ok := ap.liveDef.Collection.field(in.Field)
		if !ok || (f.Type != TypeAttachment && f.Type != TypeSignature) {
			return invalidf("the app has no attachment or signature field %q", in.Field)
		}
		maxBytes := f.maxFileBytes()
		if in.Size < 1 || in.Size > maxBytes {
			return invalidAs(apierr.CodeAppUploadSize, apierr.Params{"field": f.Name, "max": maxBytes},
				"%s takes files of 1 to %d bytes", f.Name, maxBytes)
		}
		var pending int
		var total int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM app_attachments WHERE app_id = ? AND owner = ? AND row_id IS NULL`, appID, a.UserID).Scan(&pending); err != nil {
			return err
		}
		if pending >= MaxPendingAttachments {
			return &fullError{msg: fmt.Sprintf("at most %d photos waiting to be saved in a record", MaxPendingAttachments),
				code: apierr.CodeAppUploadsPending, params: apierr.Params{"max": MaxPendingAttachments}}
		}
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(size), 0) FROM app_attachments WHERE app_id = ?`, appID).Scan(&total); err != nil {
			return err
		}
		if total+in.Size > MaxAppFileBytes {
			return &fullError{msg: fmt.Sprintf("an app's attachments take at most %d MiB", MaxAppFileBytes>>20),
				code: apierr.CodeAppFilesFull, params: apierr.Params{"max_mib": MaxAppFileBytes >> 20}}
		}
		// The file's bytes, and the thumbnail finalizing will add, are the space's from now on.
		if err := s.checkQuota(ctx, tx, sp, in.Size); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO app_attachments (id, app_id, space_id, field, kind, owner, status, size) VALUES (?, ?, ?, ?, ?, ?, 'uploading', ?)`,
			id, appID, sp.ID, f.Name, f.Type, a.UserID, in.Size); err != nil {
			return err
		}
		u, err := loadAttachment(ctx, tx, appID, id)
		created = u.Attachment
		return err
	})
	created.ChunkSize, created.MaxChunk = ChunkSize, MaxChunk
	return created, err
}

// uploadState tells the uploader where the server is in an upload: Received is the offset to
// continue from.
func (s *Service) uploadState(ctx context.Context, sp access.Space, a Actor, appID int64, id string) (Attachment, error) {
	if err := may(sp, access.Write); err != nil {
		return Attachment{}, err
	}
	var out Attachment
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := s.uploader(ctx, a, sp.ID, appID); err != nil {
			return err
		}
		u, err := ownUpload(ctx, tx, a, appID, id)
		out = u.Attachment
		return err
	})
	out.ChunkSize, out.MaxChunk = ChunkSize, MaxChunk
	return out, err
}

// putChunk writes data at offset of upload id, which must be where the server is.
func (s *Service) putChunk(ctx context.Context, sp access.Space, a Actor, appID int64, id string, offset int64, data []byte) (Attachment, error) {
	if err := may(sp, access.Write); err != nil {
		return Attachment{}, err
	}
	if !attachmentIDPattern.MatchString(id) {
		return Attachment{}, ErrNotFound
	}
	defer s.lock(id)()
	var out Attachment
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := s.uploader(ctx, a, sp.ID, appID); err != nil {
			return err
		}
		if _, err := launchable(ctx, tx, sp.ID, appID); err != nil {
			return err
		}
		u, err := ownUpload(ctx, tx, a, appID, id)
		if err != nil {
			return err
		}
		if u.Status != statusUploading {
			return errUploadState
		}
		if offset != u.Received {
			return offsetError(u.Received)
		}
		if len(data) == 0 {
			return invalidf("a chunk has at least one byte")
		}
		if u.Received+int64(len(data)) > u.Size {
			return &statusError{http.StatusRequestEntityTooLarge, apierr.CodeAppUploadChunk, "the chunk runs past the size the upload declared",
				apierr.Params{"max": u.Size - u.Received}}
		}
		root, err := s.filesRoot(ctx, u.spaceID)
		if err != nil {
			return err
		}
		defer root.Close()
		if err := root.MkdirAll(path.Dir(fileName(appID, id, "")), 0o700); err != nil {
			return err
		}
		f, err := root.OpenFile(fileName(appID, id, ".part"), os.O_WRONLY|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		// At the offset, not appended: a chunk written before and not recorded (the server stopped
		// in between) is overwritten by the same bytes, never doubled.
		if _, err := f.WriteAt(data, offset); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE app_attachments SET received = received + ?, updated_at = ? WHERE id = ?`, len(data), now(), id); err != nil {
			return err
		}
		u, err = loadAttachment(ctx, tx, appID, id)
		out = u.Attachment
		return err
	})
	return out, err
}

// finalizeUpload turns a complete upload into an attachment, or answers where the server is if it
// isn't complete. A ready attachment is answered as it is, so finalizing again is safe.
func (s *Service) finalizeUpload(ctx context.Context, sp access.Space, a Actor, appID int64, id string) (Attachment, error) {
	if err := may(sp, access.Write); err != nil {
		return Attachment{}, err
	}
	if !attachmentIDPattern.MatchString(id) {
		return Attachment{}, ErrNotFound
	}
	defer s.lock(id)()
	var u upload
	var signature bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := s.uploader(ctx, a, sp.ID, appID); err != nil {
			return err
		}
		ap, err := launchable(ctx, tx, sp.ID, appID)
		if err != nil {
			return err
		}
		u, err = ownUpload(ctx, tx, a, appID, id)
		if err == nil && ap.liveDef != nil {
			f, _ := ap.liveDef.Collection.field(u.Field)
			signature = f.Type == TypeSignature
		}
		return err
	})
	if err != nil {
		return Attachment{}, err
	}
	if u.Status == statusReady {
		return u.Attachment, nil
	}
	if u.Received != u.Size {
		return Attachment{}, offsetError(u.Received)
	}
	root, err := s.filesRoot(ctx, u.spaceID)
	if err != nil {
		return Attachment{}, err
	}
	defer root.Close()
	data, err := readUpload(root, appID, id, u.Size)
	if errors.Is(err, errShort) || errors.Is(err, fs.ErrNotExist) {
		// The bytes the store counts aren't all in the file (it was lost): start again from what is.
		have := int64(0)
		if fi, serr := root.Stat(fileName(appID, id, ".part")); serr == nil {
			have = min(fi.Size(), u.Size)
		}
		_, werr := s.db.ExecContext(ctx, `UPDATE app_attachments SET received = ?, updated_at = ? WHERE id = ? AND status = 'uploading'`, have, now(), id)
		return Attachment{}, errors.Join(offsetError(have), werr)
	}
	if err != nil {
		return Attachment{}, err
	}
	pic, err := inspect(data)
	if err == nil && signature && pic.format != "png" {
		err = errNotPNG
	}
	var thumb, stored []byte
	if err == nil {
		thumb, err = s.makeThumbnail(ctx, data, pic.format)
	}
	if err == nil {
		// What is kept is the picture without the metadata a camera wrote into it (where it was
		// taken, and more): the readers of a record see the picture, not that.
		stored, err = stripMetadata(data, pic.format)
	}
	if err == nil {
		// A picture the stripped bytes don't show as the received ones did is not kept.
		var again sniffed
		if again, err = inspect(stored); err == nil && again != pic {
			err = errNotPicture
		}
	}
	if errors.Is(err, errNotPicture) || errors.Is(err, errNotPNG) {
		// Not a picture (or, for a signature, not a PNG), so not an attachment: it is removed, and the
		// client starts over.
		if derr := s.deleteAttachment(ctx, id, appID); derr != nil {
			slog.Warn("apps: removing a file that isn't a picture", "app", appID, "err", derr)
		}
		return Attachment{}, err
	}
	if err != nil {
		return Attachment{}, err
	}
	// What is kept is the picture and its thumbnail, which can be more than the size the upload
	// declared (and reserved): the difference must fit.
	if grow := int64(len(stored)) + int64(len(thumb)) - u.Size; grow > 0 {
		if err := s.checkQuota(ctx, s.db, sp, grow); err != nil {
			return Attachment{}, err
		}
	}
	// The order makes it safe to stop at any point. The received file (.part) is removed only once
	// the store says the attachment is ready, so until then a retry finds it and does all this again;
	// the thumbnail and the stored picture are each written whole or not at all. Bytes past the
	// declared size (a chunk written and never recorded, the server having stopped in between) were
	// not read, and are not the picture's.
	if err := writeFile(root, fileName(appID, id, ".thumb"), thumb); err != nil {
		return Attachment{}, err
	}
	if err := writeFile(root, fileName(appID, id, ""), stored); err != nil {
		return Attachment{}, err
	}
	var ready Attachment
	err = s.write(ctx, func(tx *sql.Tx) error {
		if err := s.uploader(ctx, a, sp.ID, appID); err != nil {
			return err
		}
		// From here the size is the stored one (the length it is served with), no longer the declared one.
		res, err := tx.ExecContext(ctx, `
			UPDATE app_attachments SET status = 'ready', size = ?, received = ?, content_type = ?, width = ?, height = ?,
				thumb_size = ?, updated_at = ?
			WHERE id = ? AND app_id = ? AND owner = ? AND status = 'uploading'`,
			len(stored), len(stored), pic.contentType, pic.width, pic.height, len(thumb), now(), id, appID, a.UserID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound // removed meanwhile
		}
		u, err := loadAttachment(ctx, tx, appID, id)
		ready = u.Attachment
		return err
	})
	if err != nil {
		return ready, err
	}
	if err := root.Remove(fileName(appID, id, ".part")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("apps: removing a received upload", "app", appID, "err", err) // Sweep removes it
	}
	return ready, nil
}

var errShort = errors.New("the file has fewer bytes than were received")

// readUpload reads the size bytes of an upload from the file being received. Only that file has
// them: the finished one, which finalizing may have written before it stopped, holds the picture
// without its metadata, so it is no stand-in, and finalizing starts again from this one.
func readUpload(root *os.Root, appID int64, id string, size int64) ([]byte, error) {
	f, err := root.Open(fileName(appID, id, ".part"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data := make([]byte, size)
	if _, err := io.ReadFull(f, data); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errShort
		}
		return nil, err
	}
	return data, nil
}

// writeFile writes data to name in root, whole or not at all: to a temporary file renamed into
// place.
func writeFile(root *os.Root, name string, data []byte) error {
	tmp := name + ".tmp"
	if err := root.MkdirAll(path.Dir(name), 0o700); err != nil {
		return err
	}
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}

// makeThumbnail makes a thumbnail with at most maxThumbnails pictures being decoded at a time.
func (s *Service) makeThumbnail(ctx context.Context, data []byte, format string) ([]byte, error) {
	select {
	case s.thumbs <- struct{}{}:
		defer func() { <-s.thumbs }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return thumbnail(data, format)
}

// abandonUpload removes an attachment its uploader no longer wants, before it is attached to a
// record: an upload on its way, or a photo they took out of the form.
func (s *Service) abandonUpload(ctx context.Context, sp access.Space, a Actor, appID int64, id string) error {
	if err := may(sp, access.Write); err != nil {
		return err
	}
	if !attachmentIDPattern.MatchString(id) {
		return ErrNotFound
	}
	defer s.lock(id)()
	var u upload
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := s.uploader(ctx, a, sp.ID, appID); err != nil {
			return err
		}
		var err error
		if u, err = ownUpload(ctx, tx, a, appID, id); err != nil {
			return err
		}
		if u.Attached {
			return errUploadState // a record holds it: it stays
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM app_attachments WHERE id = ? AND row_id IS NULL`, id)
		return err
	})
	if err != nil {
		return err
	}
	s.removeFiles(ctx, u.spaceID, appID, id)
	return nil
}

// deleteAttachment removes an attachment that isn't attached, whoever's: the server's own clearing
// of what can't be used.
func (s *Service) deleteAttachment(ctx context.Context, id string, appID int64) error {
	var spaceID int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `DELETE FROM app_attachments WHERE id = ? AND row_id IS NULL RETURNING space_id`, id).Scan(&spaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil || spaceID == 0 {
		return err
	}
	s.removeFiles(ctx, spaceID, appID, id)
	return nil
}

// removeFiles deletes an attachment's files from the space that held them. A failure is logged and
// leaves a stray file, which Sweep removes later.
func (s *Service) removeFiles(ctx context.Context, spaceID, appID int64, id string) {
	root, err := s.filesRoot(ctx, spaceID)
	if err != nil {
		slog.Warn("apps: removing attachment files", "app", appID, "err", err)
		return
	}
	defer root.Close()
	for _, suffix := range []string{"", ".part", ".thumb", ".thumb.tmp", ".tmp"} {
		if err := root.Remove(fileName(appID, id, suffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("apps: removing attachment file", "app", appID, "err", err)
		}
	}
}

// File is an attachment (or its thumbnail) opened for reading.
type File struct {
	*os.File
	ContentType string
	// Name is the file name offered to a browser: made here, from the id and the type.
	Name string
	ETag string
}

// openAttachment opens attachment id of appID, or its thumbnail, for a caller who may read it:
// those who read the record it is attached to (a live record, within their role's records), or,
// before it is attached, its uploader. Whatever else, the answer is ErrNotFound.
func (s *Service) openAttachment(ctx context.Context, sp access.Space, a Actor, appID int64, id string, thumb bool) (*File, error) {
	if err := may(sp, access.Read); err != nil {
		return nil, err
	}
	acc, err := s.appAccess(ctx, a, sp.ID, appID)
	if err != nil {
		return nil, err
	}
	u, err := loadAttachment(ctx, s.db, appID, id)
	if err != nil {
		return nil, err
	}
	if u.Status != statusReady {
		return nil, ErrNotFound
	}
	if u.rowID.Valid {
		var creator sql.NullInt64
		var deleted sql.NullString
		if err := s.db.QueryRowContext(ctx, `SELECT created_by, deleted_at FROM app_rows WHERE id = ? AND app_id = ?`, u.rowID.Int64, appID).Scan(&creator, &deleted); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, err
		}
		if deleted.Valid || !readable(acc, a.UserID).sees(creator) {
			return nil, ErrNotFound
		}
	} else if !u.owner.Valid || u.owner.Int64 != a.UserID {
		return nil, ErrNotFound
	}
	root, err := s.filesRoot(ctx, u.spaceID)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	suffix, contentType, ext := "", u.ContentType, ""
	for _, pt := range pictureTypes {
		if pt.contentType == u.ContentType {
			ext = pt.ext
		}
	}
	etag := `"` + id + `"`
	if thumb {
		suffix, contentType, ext, etag = ".thumb", "image/jpeg", "jpg", `"`+id+`-thumb"`
	}
	f, err := root.Open(fileName(appID, id, suffix))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &File{File: f, ContentType: contentType, Name: "photo-" + id[:8] + "." + ext, ETag: etag}, nil
}

// attachmentIDs reads the ids a stored attachment field holds, or the one a signature field does.
func attachmentIDs(v any) []string {
	if id, ok := v.(string); ok {
		return []string{id}
	}
	list, _ := v.([]any)
	ids := make([]string, 0, len(list))
	for _, item := range list {
		if id, ok := item.(string); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// claimAttachments attaches the attachments row rowID of the app's collection newly lists
// (after, against before, the row as it was; nil for an insert) to it, in the transaction that
// writes the row. An id that isn't yet the row's must be an attachment of this app and field,
// uploaded by the actor, finished and attached to nothing, or the change is refused as a whole.
// The refusal doesn't say which of those it was: an id of someone else's isn't told from one that
// doesn't exist.
func claimAttachments(ctx context.Context, tx *sql.Tx, actor Actor, appID, rowID int64, def Definition, before, after map[string]any) error {
	for _, f := range def.Collection.Fields {
		if f.Type != TypeAttachment && f.Type != TypeSignature {
			continue
		}
		// A signature is a PNG, by what the bytes turned out to be, and an attachment is only claimed by
		// a field of the kind it was uploaded for: a field retyped since can't take what was pending
		// for what it was. What the field's size limit is now holds too, whatever it was at upload.
		onlyType := ""
		if f.Type == TypeSignature {
			onlyType = "image/png"
		}
		limit := f.maxFileBytes()
		had := map[string]bool{}
		for _, id := range attachmentIDs(before[f.Name]) {
			had[id] = true
		}
		for _, id := range attachmentIDs(after[f.Name]) {
			if had[id] {
				continue
			}
			res, err := tx.ExecContext(ctx, `
				UPDATE app_attachments SET row_id = ?, attached_at = ?
				WHERE id = ? AND app_id = ? AND field = ? AND kind = ? AND owner = ? AND status = 'ready' AND row_id IS NULL
					AND (? = '' OR content_type = ?) AND size <= ?`,
				rowID, now(), id, appID, f.Name, f.Type, actor.UserID, onlyType, onlyType, limit)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return invalidAs(apierr.CodeAppAttachmentBad, apierr.Params{"field": f.Name},
					"%s: a photo can't be used here: it must be one you uploaded for this field, finished, within its size limit, and not in a record already", f.Name)
			}
		}
	}
	return nil
}

// maxFileBytes is the most a file of the field may have: its max_bytes, or the default of its kind.
func (f Field) maxFileBytes() int64 {
	if f.MaxBytes != 0 {
		return int64(f.MaxBytes)
	}
	if f.Type == TypeSignature {
		return DefaultSignatureBytes
	}
	return DefaultMaxFileBytes
}

// filesModel is the limits of the collection's attachment and signature fields, for the screen's
// data model at /_files (nil if there are none).
func (d Definition) filesModel() map[string]any {
	var out map[string]any
	for _, f := range d.Collection.Fields {
		if f.Type != TypeAttachment && f.Type != TypeSignature {
			continue
		}
		if out == nil {
			out = map[string]any{}
		}
		if f.Type == TypeSignature {
			out[f.Name] = map[string]any{"max_files": 1, "max_bytes": cmpOr(f.MaxBytes, DefaultSignatureBytes)}
			continue
		}
		out[f.Name] = map[string]any{"max_files": cmpOr(f.MaxFiles, DefaultMaxFiles), "max_bytes": cmpOr(f.MaxBytes, DefaultMaxFileBytes)}
	}
	return out
}

// fieldsModel is what a screen needs to know of the collection's capture fields, for the data model
// at /_fields/<field> (nil if there are none): when a location is taken and whether it can still be
// changed once stored, the format a barcode reads, and whether the field must have a value.
func (d Definition) fieldsModel() map[string]any {
	var out map[string]any
	for _, f := range d.Collection.Fields {
		var info map[string]any
		switch f.Type {
		case TypeLocation:
			info = map[string]any{"capture": cmpOrString(f.Capture, CaptureButton)}
		case TypeBarcode:
			info = map[string]any{"format": f.Format}
		case TypeSignature:
		default:
			continue
		}
		if info == nil {
			info = map[string]any{}
		}
		info["required"] = f.Required
		info["editable"] = f.Editable
		if out == nil {
			out = map[string]any{}
		}
		out[f.Name] = info
	}
	return out
}

func cmpOrString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func cmpOr(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

// parseAttachments reads an attachment field's value: a list of attachment ids (or one), at most
// the field's MaxFiles, each different. Whether they may be used is for claimAttachments.
func (f Field) parseAttachments(v any) (any, bool, error) {
	var list []any
	switch x := v.(type) {
	case []any:
		list = x
	case string:
		list = []any{x}
	default:
		return nil, false, invalidAs(apierr.CodeAppFieldFiles, apierr.Params{"field": f.Name, "max": cmpOr(f.MaxFiles, DefaultMaxFiles)},
			"%s must be a list of attachments", f.Name)
	}
	if len(list) == 0 {
		return nil, false, nil
	}
	limit := cmpOr(f.MaxFiles, DefaultMaxFiles)
	out := make([]any, 0, len(list))
	seen := map[string]bool{}
	for _, item := range list {
		id, ok := item.(string)
		if !ok || !attachmentIDPattern.MatchString(id) || seen[id] {
			return nil, false, invalidAs(apierr.CodeAppFieldFiles, apierr.Params{"field": f.Name, "max": limit},
				"%s must be a list of different attachments", f.Name)
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) > limit {
		return nil, false, invalidAs(apierr.CodeAppFieldFiles, apierr.Params{"field": f.Name, "max": limit},
			"%s takes at most %d attachments", f.Name, limit)
	}
	return out, true, nil
}

// Described is an attachment as the agent reads it: its metadata, never its bytes.
type Described struct {
	ID          string `json:"id"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
}

// describeAttachments replaces, in items (as Items made them), the ids an attachment field holds
// by what each is: id, type, size and pixels. It never reads a file.
//
// spaceID, if not 0, says the app must be in that space: the same statement checks it.
func describeAttachments(ctx context.Context, q queryer, spaceID, appID int64, def Definition, items []map[string]any) error {
	var names []string
	single := map[string]bool{}
	for _, f := range def.Collection.Fields {
		if f.Type == TypeAttachment || f.Type == TypeSignature {
			names = append(names, f.Name)
			single[f.Name] = f.Type == TypeSignature
		}
	}
	if len(names) == 0 {
		return nil
	}
	known := map[string]Described{}
	rows, err := q.QueryContext(ctx, `SELECT t.id, t.content_type, t.size, coalesce(t.width, 0), coalesce(t.height, 0) FROM app_attachments t
		WHERE t.app_id = ? AND t.status = 'ready'
		AND (? = 0 OR EXISTS (SELECT 1 FROM apps a WHERE a.id = t.app_id AND a.space_id = ?))`, appID, spaceID, spaceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var d Described
		if err := rows.Scan(&d.ID, &d.ContentType, &d.Size, &d.Width, &d.Height); err != nil {
			return err
		}
		known[d.ID] = d
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, it := range items {
		for _, name := range names {
			if v, ok := it[name]; ok {
				var list []Described
				for _, id := range attachmentIDs(v) {
					d, ok := known[id]
					if !ok {
						d = Described{ID: id}
					}
					list = append(list, d)
				}
				if single[name] && len(list) == 1 {
					it[name] = list[0] // a signature is one picture
					continue
				}
				it[name] = list
			}
		}
	}
	return nil
}

// relocate moves the files of app appID's attachments to the space the app is in now, after it
// was moved: each is copied, its row says where it is, and the old copy is removed. It can be run
// again at any point, and reads are right at every step, since they follow the row.
func (s *Service) relocate(ctx context.Context, appID int64) error {
	if s.dataDir == "" {
		return nil
	}
	var home int64
	if err := s.db.QueryRowContext(ctx, `SELECT space_id FROM apps WHERE id = ?`, appID).Scan(&home); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, space_id FROM app_attachments WHERE app_id = ? AND space_id != ?`, appID, home)
	if err != nil {
		return err
	}
	type move struct {
		id    string
		space int64
	}
	var moves []move
	for rows.Next() {
		var m move
		if err := rows.Scan(&m.id, &m.space); err != nil {
			_ = rows.Close()
			return err
		}
		moves = append(moves, m)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var errs []error
	for _, m := range moves {
		errs = append(errs, s.relocateOne(ctx, appID, m.id, m.space, home))
	}
	return errors.Join(errs...)
}

func (s *Service) relocateOne(ctx context.Context, appID int64, id string, from, to int64) error {
	defer s.lock(id)()
	src, err := s.filesRoot(ctx, from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := s.filesRoot(ctx, to)
	if err != nil {
		return err
	}
	defer dst.Close()
	// The stored picture and its thumbnail go. The received file goes only while the upload isn't
	// finished: after, it is a leftover that is not the picture (it still has its metadata).
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM app_attachments WHERE id = ?`, id).Scan(&status); err != nil {
		return err
	}
	suffixes := []string{"", ".part", ".thumb"}
	for _, suffix := range suffixes {
		if suffix == ".part" && status == statusReady {
			continue
		}
		data, err := readWhole(src, fileName(appID, id, suffix))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := writeFile(dst, fileName(appID, id, suffix), data); err != nil {
			return err
		}
	}
	if err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE app_attachments SET space_id = ? WHERE id = ? AND space_id = ?`, to, id, from)
		return err
	}); err != nil {
		return err
	}
	for _, suffix := range suffixes {
		if err := src.Remove(fileName(appID, id, suffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("apps: removing the old copy of an attachment", "app", appID, "err", err)
		}
	}
	return nil
}

func readWhole(root *os.Root, name string) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, MaxMaxFileBytes+1))
}

// Sweep removes what has outlived its use, as of cutoff: attachments attached to no record that
// haven't changed since cutoff (abandoned uploads and photos never submitted), with their files;
// the files of attachments whose app was moved, copied to the app's space; and files that no
// attachment row of their space knows (what a failed removal left, an app's that is gone) that
// haven't changed since cutoff. Records' attachments are never touched.
func (s *Service) Sweep(ctx context.Context, cutoff time.Time) error {
	if s.dataDir == "" {
		return nil
	}
	errs := []error{s.sweep(ctx, cutoff, 0)}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT a.id FROM app_attachments t JOIN apps a ON a.id = t.app_id WHERE t.space_id != a.space_id`)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	var moved []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return errors.Join(append(errs, err)...)
		}
		moved = append(moved, id)
	}
	errs = append(errs, rows.Err(), rows.Close())
	for _, id := range moved {
		errs = append(errs, s.relocate(ctx, id))
	}
	errs = append(errs, s.sweepStrays(ctx, cutoff))
	return errors.Join(errs...)
}

// sweep removes the unattached attachments of appID (every app if 0) not changed since cutoff.
func (s *Service) sweep(ctx context.Context, cutoff time.Time, appID int64) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, app_id FROM app_attachments
		WHERE row_id IS NULL AND updated_at < ? AND (? = 0 OR app_id = ?) LIMIT 500`, cutoff.UTC().Format(timeFormat), appID, appID)
	if err != nil {
		return err
	}
	type old struct {
		id  string
		app int64
	}
	var olds []old
	for rows.Next() {
		var o old
		if err := rows.Scan(&o.id, &o.app); err != nil {
			_ = rows.Close()
			return err
		}
		olds = append(olds, o)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var errs []error
	for _, o := range olds {
		errs = append(errs, s.deleteExpired(ctx, o.id, o.app, cutoff))
	}
	return errors.Join(errs...)
}

// deleteExpired removes attachment id if it still is unattached and unchanged since cutoff: it
// may have been attached, or have taken a chunk, since it was found.
func (s *Service) deleteExpired(ctx context.Context, id string, appID int64, cutoff time.Time) error {
	defer s.lock(id)()
	var spaceID int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `DELETE FROM app_attachments WHERE id = ? AND row_id IS NULL AND updated_at < ? RETURNING space_id`,
			id, cutoff.UTC().Format(timeFormat)).Scan(&spaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil || spaceID == 0 {
		return err
	}
	s.removeFiles(ctx, spaceID, appID, id)
	return nil
}

// sweepStrays removes files of attachments no row knows, and the directories of apps that are
// gone, in every space, if they haven't changed since cutoff.
func (s *Service) sweepStrays(ctx context.Context, cutoff time.Time) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, workspace_id FROM spaces`)
	if err != nil {
		return err
	}
	type space struct{ id, workspace int64 }
	var spaces []space
	for rows.Next() {
		var sp space
		if err := rows.Scan(&sp.id, &sp.workspace); err != nil {
			_ = rows.Close()
			return err
		}
		spaces = append(spaces, sp)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var errs []error
	for _, sp := range spaces {
		if _, err := os.Stat(filepath.Join(access.SpaceDir(s.dataDir, sp.workspace, sp.id), filesDir)); err != nil {
			continue // no files, or nothing to see
		}
		errs = append(errs, s.sweepSpace(ctx, access.Space{ID: sp.id, WorkspaceID: sp.workspace}, cutoff))
	}
	return errors.Join(errs...)
}

func (s *Service) sweepSpace(ctx context.Context, sp access.Space, cutoff time.Time) error {
	root, err := access.OpenRoot(s.dataDir, sp)
	if err != nil {
		return err
	}
	defer root.Close()
	apps, err := fs.ReadDir(root.FS(), filesDir)
	if err != nil {
		return err
	}
	var errs []error
	for _, dir := range apps {
		appID, err := strconv.ParseInt(dir.Name(), 10, 64)
		if err != nil || !dir.IsDir() {
			continue
		}
		name := path.Join(filesDir, dir.Name())
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM apps WHERE id = ?`, appID).Scan(&n); err != nil {
			errs = append(errs, err)
			continue
		}
		if n == 0 {
			if fi, err := dir.Info(); err == nil && fi.ModTime().Before(cutoff) {
				errs = append(errs, root.RemoveAll(name))
			}
			continue
		}
		files, err := fs.ReadDir(root.FS(), name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, f := range files {
			id, suffix, _ := strings.Cut(f.Name(), ".")
			// The received file of an attachment that is ready is a leftover of finalizing (it was
			// stopped, or couldn't remove it): it holds the original, metadata included, and nothing
			// reads it, so it goes at once.
			if suffix == "part" && attachmentIDPattern.MatchString(id) {
				var ready int
				if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM app_attachments WHERE id = ? AND space_id = ? AND status = 'ready'`,
					id, sp.ID).Scan(&ready); err != nil {
					errs = append(errs, err)
					continue
				}
				if ready > 0 {
					if err := root.Remove(path.Join(name, f.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
						errs = append(errs, err)
					}
					continue
				}
			}
			fi, err := f.Info()
			if err != nil || !attachmentIDPattern.MatchString(id) || !fi.ModTime().Before(cutoff) {
				continue
			}
			var known int
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM app_attachments WHERE id = ? AND space_id = ?`, id, sp.ID).Scan(&known); err != nil {
				errs = append(errs, err)
				continue
			}
			if known == 0 {
				if err := root.Remove(path.Join(name, f.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// Maintain runs Sweep every interval until ctx is done, as the server does for the files it keeps.
func (s *Service) Maintain(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.Sweep(ctx, time.Now().Add(-AttachmentTTL)); err != nil && ctx.Err() == nil {
			slog.Warn("apps: sweeping attachments", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
