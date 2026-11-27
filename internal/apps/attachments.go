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

const (
	ChunkSize             = 1 << 20
	MaxChunk              = 2 << 20
	MaxPendingAttachments = 20
	MaxAppFileBytes       = 1 << 30
	AttachmentTTL         = 24 * time.Hour
	maxThumbnails         = 2
)

const (
	statusUploading = "uploading"
	statusReady     = "ready"
)

const filesDir = access.ManagedDir + "/app-files"

var attachmentIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

var errNotPicture = invalidAs(apierr.CodeAppAttachmentType, apierr.Params{"max_megapixels": maxPixels["jpeg"] / 1_000_000},
	"the file isn't a JPEG, PNG or GIF picture this server can read")

var errNotPNG = invalidAs(apierr.CodeAppSignatureType, nil, "a signature is a PNG picture")

type statusError struct {
	status int
	code   apierr.Code
	msg    string
	params apierr.Params
}

func (e *statusError) Error() string { return e.msg }

var errNoFiles = &statusError{http.StatusServiceUnavailable, apierr.CodeAppFilesUnavailable, "this server keeps no files for apps", nil}

type Attachment struct {
	ID          string `json:"id"`
	Field       string `json:"field"`
	Status      string `json:"status"`
	Size        int64  `json:"size"`
	Received    int64  `json:"received"`
	ContentType string `json:"content_type,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	Attached    bool   `json:"attached"`
	CreatedAt   string `json:"created_at"`
	ChunkSize   int    `json:"chunk_size,omitempty"`
	MaxChunk    int    `json:"max_chunk,omitempty"`
}

type NewUpload struct {
	Field string `json:"field"`
	Size  int64  `json:"size"`
}

func (s *Service) WithFiles(dataDir string) *Service {
	s.dataDir = dataDir
	return s
}

type SpaceHolder interface {
	HoldSpace(sp access.Space) (release func(), err error)
}

func (s *Service) WithSpaceHolder(h SpaceHolder) *Service {
	s.holder = h
	return s
}

func (s *Service) WithQuota(q *quota.Service) *Service {
	s.quota = q
	return s
}

func (s *Service) reserveQuota(ctx context.Context, q quota.Queryer, sp access.Space, adding int64) (*quota.Reservation, error) {
	if s.quota == nil {
		return nil, nil
	}
	return s.quota.Reserve(ctx, q, sp, adding)
}

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

func (s *Service) filesRoot(ctx context.Context, spaceID int64) (root *os.Root, done func(), err error) {
	return s.filesRootAs(ctx, spaceID, "")
}

func (s *Service) filesRootAs(ctx context.Context, spaceID int64, created string) (root *os.Root, done func(), err error) {
	if s.dataDir == "" {
		return nil, nil, errNoFiles
	}
	var workspace int64
	var row string
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_id, created_at FROM spaces WHERE id = ?`, spaceID).Scan(&workspace, &row); err != nil {
		return nil, nil, goneAsNotFound(err)
	}
	if created != "" && row != created {
		return nil, nil, goneAsNotFound(sql.ErrNoRows)
	}
	sp := access.Space{ID: spaceID, WorkspaceID: workspace, CreatedAt: row}
	release := func() {}
	if s.holder != nil {
		if release, err = s.holder.HoldSpace(sp); err != nil {
			return nil, nil, fmt.Errorf("%w: %w", ErrNotFound, err)
		}
	}
	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM spaces WHERE id = ? AND workspace_id = ? AND created_at = ?`,
		sp.ID, sp.WorkspaceID, row).Scan(&one); err != nil {
		release()
		return nil, nil, goneAsNotFound(err)
	}
	root, err = access.OpenRoot(s.dataDir, sp)
	if err != nil {
		release()
		return nil, nil, err
	}
	return root, func() { _ = root.Close(); release() }, nil
}

func goneAsNotFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}

func fileName(appID int64, id, suffix string) string {
	return path.Join(filesDir, strconv.FormatInt(appID, 10), id+suffix)
}

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
	if err := s.sweep(ctx, time.Now().Add(-AttachmentTTL), appID); err != nil {
		slog.Warn("apps: removing expired attachments", "app", appID, "err", err)
	}
	var created Attachment
	var held *quota.Reservation
	defer func() { held.Release() }()
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
		var qerr error
		if held, qerr = s.reserveQuota(ctx, tx, sp, in.Size); qerr != nil {
			return qerr
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

func (s *Service) putChunk(ctx context.Context, sp access.Space, a Actor, appID int64, id string, offset int64, data []byte) (Attachment, error) {
	if err := may(sp, access.Write); err != nil {
		return Attachment{}, err
	}
	if !attachmentIDPattern.MatchString(id) {
		return Attachment{}, ErrNotFound
	}
	defer s.lock(id)()
	var held *os.Root
	var heldSpace int64
	var rootErr error
	if err := s.db.QueryRowContext(ctx, `SELECT space_id FROM app_attachments WHERE id = ? AND app_id = ?`, id, appID).Scan(&heldSpace); err == nil {
		root, done, err := s.filesRoot(ctx, heldSpace)
		if err != nil {
			rootErr = err
		} else {
			defer done()
			held = root
		}
	}
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
		if held == nil {
			if rootErr != nil {
				return rootErr
			}
			return ErrNotFound
		}
		if u.spaceID != heldSpace {
			return errUploadState
		}
		root := held
		if err := root.MkdirAll(path.Dir(fileName(appID, id, "")), 0o700); err != nil {
			return err
		}
		f, err := root.OpenFile(fileName(appID, id, ".part"), os.O_WRONLY|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
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
	root, done, err := s.filesRoot(ctx, u.spaceID)
	if err != nil {
		return Attachment{}, err
	}
	defer done()
	data, err := readUpload(root, appID, id, u.Size)
	if errors.Is(err, errShort) || errors.Is(err, fs.ErrNotExist) {
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
		stored, err = stripMetadata(data, pic.format)
	}
	if err == nil {
		var again sniffed
		if again, err = inspect(stored); err == nil && again != pic {
			err = errNotPicture
		}
	}
	if errors.Is(err, errNotPicture) || errors.Is(err, errNotPNG) {
		if derr := s.deleteAttachment(ctx, id, appID, root); derr != nil {
			slog.Warn("apps: removing a file that isn't a picture", "app", appID, "err", derr)
		}
		return Attachment{}, err
	}
	if err != nil {
		return Attachment{}, err
	}
	if err := writeFile(root, fileName(appID, id, ".thumb"), thumb); err != nil {
		return Attachment{}, err
	}
	if err := writeFile(root, fileName(appID, id, ""), stored); err != nil {
		return Attachment{}, err
	}
	var ready Attachment
	var held *quota.Reservation
	defer func() { held.Release() }()
	err = s.write(ctx, func(tx *sql.Tx) error {
		if err := s.uploader(ctx, a, sp.ID, appID); err != nil {
			return err
		}
		var declared int64
		if err := tx.QueryRowContext(ctx, `SELECT size FROM app_attachments WHERE id = ? AND app_id = ? AND owner = ? AND status = 'uploading'`,
			id, appID, a.UserID).Scan(&declared); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var qerr error
		if held, qerr = s.reserveQuota(ctx, tx, sp, int64(len(stored))+int64(len(thumb))-declared); qerr != nil {
			return qerr
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE app_attachments SET status = 'ready', size = ?, received = ?, content_type = ?, width = ?, height = ?,
				thumb_size = ?, updated_at = ?
			WHERE id = ? AND app_id = ? AND owner = ? AND status = 'uploading'`,
			len(stored), len(stored), pic.contentType, pic.width, pic.height, len(thumb), now(), id, appID, a.UserID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		u, err := loadAttachment(ctx, tx, appID, id)
		ready = u.Attachment
		return err
	})
	if errors.Is(err, quota.ErrExceeded) || errors.Is(err, quota.ErrUnavailable) {
		for _, ext := range []string{".thumb", ""} {
			if rerr := root.Remove(fileName(appID, id, ext)); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				slog.Warn("apps: removing a picture the quota refused", "app", appID, "err", rerr)
			}
		}
	}
	if err != nil {
		return ready, err
	}
	if err := root.Remove(fileName(appID, id, ".part")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("apps: removing a received upload", "app", appID, "err", err)
	}
	return ready, nil
}

var errShort = errors.New("the file has fewer bytes than were received")

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

func (s *Service) makeThumbnail(ctx context.Context, data []byte, format string) ([]byte, error) {
	select {
	case s.thumbs <- struct{}{}:
		defer func() { <-s.thumbs }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return thumbnail(data, format)
}

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
			return errUploadState
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

func (s *Service) deleteAttachment(ctx context.Context, id string, appID int64, held *os.Root) error {
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
	if held != nil {
		if err := unlinkIn(held, appID, id); err != nil {
			slog.Warn("apps: removing attachment files", "app", appID, "err", err)
		}
		return nil
	}
	s.removeFiles(ctx, spaceID, appID, id)
	return nil
}

func (s *Service) removeFiles(ctx context.Context, spaceID, appID int64, id string) {
	if err := s.unlinkFiles(ctx, spaceID, appID, id); err != nil {
		slog.Warn("apps: removing attachment files", "app", appID, "err", err)
	}
}

func (s *Service) unlinkFiles(ctx context.Context, spaceID, appID int64, id string) error {
	root, done, err := s.filesRoot(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, access.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	defer done()
	return unlinkIn(root, appID, id)
}

func unlinkIn(root *os.Root, appID int64, id string) error {
	var errs []error
	for _, suffix := range []string{"", ".part", ".thumb", ".thumb.tmp", ".tmp"} {
		if err := root.Remove(fileName(appID, id, suffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type File struct {
	*os.File
	ContentType string
	Name        string
	ETag        string
}

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
	root, done, err := s.filesRoot(ctx, u.spaceID)
	if err != nil {
		return nil, err
	}
	defer done()
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

func claimAttachments(ctx context.Context, tx *sql.Tx, actor Actor, appID, rowID int64, def Definition, before, after map[string]any) error {
	for _, f := range def.Collection.Fields {
		if f.Type != TypeAttachment && f.Type != TypeSignature {
			continue
		}
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

func (f Field) maxFileBytes() int64 {
	if f.MaxBytes != 0 {
		return int64(f.MaxBytes)
	}
	if f.Type == TypeSignature {
		return DefaultSignatureBytes
	}
	return DefaultMaxFileBytes
}

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

type Described struct {
	ID          string `json:"id"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
}

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
					it[name] = list[0]
					continue
				}
				it[name] = list
			}
		}
	}
	return nil
}

func (s *Service) relocate(ctx context.Context, appID int64) error {
	if s.dataDir == "" {
		return nil
	}
	var home int64
	var homeCreated string
	if err := s.db.QueryRowContext(ctx, `SELECT a.space_id, s.created_at FROM apps a JOIN spaces s ON s.id = a.space_id WHERE a.id = ?`, appID).Scan(&home, &homeCreated); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.space_id, s.created_at FROM app_attachments t JOIN spaces s ON s.id = t.space_id
		WHERE t.app_id = ? AND t.space_id != ?`, appID, home)
	if err != nil {
		return err
	}
	type move struct {
		id      string
		space   int64
		created string
	}
	var moves []move
	for rows.Next() {
		var m move
		if err := rows.Scan(&m.id, &m.space, &m.created); err != nil {
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
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		errs = append(errs, s.relocateOne(ctx, appID, m.id, m.space, m.created, home, homeCreated))
	}
	return errors.Join(errs...)
}

func (s *Service) relocateOne(ctx context.Context, appID int64, id string, from int64, fromCreated string, to int64, toCreated string) error {
	defer s.lock(id)()
	if from == to {
		return nil
	}
	var src, dst *os.Root
	first, second := &src, &dst
	ids := [2]int64{from, to}
	if to < from {
		first, second = &dst, &src
		ids = [2]int64{to, from}
	}
	created := map[int64]string{from: fromCreated, to: toCreated}
	var err error
	var doneFirst, doneSecond func()
	if *first, doneFirst, err = s.filesRootAs(ctx, ids[0], created[ids[0]]); err != nil {
		return err
	}
	defer doneFirst()
	if *second, doneSecond, err = s.filesRootAs(ctx, ids[1], created[ids[1]]); err != nil {
		return err
	}
	defer doneSecond()
	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM app_attachments t JOIN apps a ON a.id = t.app_id
		WHERE t.id = ? AND t.app_id = ? AND t.space_id = ? AND a.space_id = ?`, id, appID, from, to).Scan(&current); err != nil {
		return err
	}
	if current == 0 {
		return nil
	}
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
	var moved bool
	if err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE app_attachments SET space_id = ? WHERE id = ? AND space_id = ?
			AND EXISTS (SELECT 1 FROM apps WHERE id = ? AND space_id = ?)`, to, id, from, appID, to)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		moved = n == 1
		return err
	}); err != nil {
		return err
	}
	if !moved {
		for _, suffix := range suffixes {
			if err := dst.Remove(fileName(appID, id, suffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				slog.Warn("apps: removing the copy of an attachment that did not move", "app", appID, "err", err)
			}
		}
		return nil
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
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		errs = append(errs, s.relocate(ctx, id))
	}
	errs = append(errs, s.sweepStrays(ctx, cutoff))
	return errors.Join(errs...)
}

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
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		errs = append(errs, s.deleteExpired(ctx, o.id, o.app, cutoff))
	}
	return errors.Join(errs...)
}

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
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if _, err := os.Stat(filepath.Join(access.SpaceDir(s.dataDir, sp.workspace, sp.id), filesDir)); err != nil {
			continue
		}
		errs = append(errs, s.sweepSpace(ctx, access.Space{ID: sp.id, WorkspaceID: sp.workspace}, cutoff))
	}
	return errors.Join(errs...)
}

func (s *Service) sweepSpace(ctx context.Context, sp access.Space, cutoff time.Time) error {
	root, done, err := s.filesRoot(ctx, sp.ID)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, access.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	defer done()
	apps, err := fs.ReadDir(root.FS(), filesDir)
	if err != nil {
		return err
	}
	var errs []error
	for _, dir := range apps {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
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
			if err := ctx.Err(); err != nil {
				return errors.Join(append(errs, err)...)
			}
			id, suffix, _ := strings.Cut(f.Name(), ".")
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

func (s *Service) FinishMoves(ctx context.Context, spaceID int64) error {
	if s.dataDir == "" {
		return nil
	}
	moving := func() ([]int64, error) {
		rows, err := s.db.QueryContext(ctx, `
			SELECT DISTINCT a.id FROM app_attachments t JOIN apps a ON a.id = t.app_id
			WHERE t.space_id != a.space_id AND (t.space_id = ? OR a.space_id = ?) ORDER BY a.id`, spaceID, spaceID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, rows.Err()
	}
	ids, err := moving()
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		errs = append(errs, s.relocate(ctx, id))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("finishing the move of an app's attachments out of space %d: %w", spaceID, err)
	}
	left, err := moving()
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return fmt.Errorf("apps %v still have attachments in two spaces, one of them space %d", left, spaceID)
	}
	return nil
}

func (s *Service) PurgeUploads(ctx context.Context, owner int64) (int, error) {
	return s.PurgeUploadsNoting(ctx, owner, nil)
}

func (s *Service) PurgeUploadsNoting(ctx context.Context, owner int64, removed func(ctx context.Context, tx *sql.Tx) error) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, app_id FROM app_attachments WHERE owner = ? AND row_id IS NULL`, owner)
	if err != nil {
		return 0, err
	}
	type upload struct {
		id  string
		app int64
	}
	var todo []upload
	for rows.Next() {
		var u upload
		if err := rows.Scan(&u.id, &u.app); err != nil {
			_ = rows.Close()
			return 0, err
		}
		todo = append(todo, u)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	n := 0
	for _, u := range todo {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		done, err := s.deleteUpload(ctx, u.id, u.app, owner, removed)
		if err != nil {
			return n, err
		}
		if done {
			n++
		}
	}
	return n, nil
}

func (s *Service) deleteUpload(ctx context.Context, id string, appID, owner int64, removed func(context.Context, *sql.Tx) error) (bool, error) {
	defer s.lock(id)()
	var spaceID int64
	err := s.db.QueryRowContext(ctx, `SELECT space_id FROM app_attachments WHERE id = ? AND owner = ? AND row_id IS NULL`,
		id, owner).Scan(&spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := s.unlinkFiles(ctx, spaceID, appID, id); err != nil {
		return false, fmt.Errorf("removing the files of upload %s: %w", id, err)
	}
	var gone bool
	err = s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM app_attachments WHERE id = ? AND owner = ? AND row_id IS NULL`, id, owner)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 && removed != nil {
			if err := removed(ctx, tx); err != nil {
				return err
			}
		}
		gone = n > 0
		return nil
	})
	return gone, err
}
