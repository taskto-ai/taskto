package apps

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The strings that stand for what a camera wrote into a photo; none may be in what is served.
var secrets = []string{"SECRET-GPS-COORDS", "SECRET-XMP", "SECRET-COMMENT", "SECRET-IPTC", "SECRET-TEXT", "SECRET-TIME", "SECRET-APP"}

func noSecrets(t *testing.T, what string, data []byte) {
	t.Helper()
	for _, s := range secrets {
		if bytes.Contains(data, []byte(s)) {
			t.Errorf("%s still holds %q", what, s)
		}
	}
}

// jpegSegment is a JPEG segment: marker, length, payload.
func jpegSegment(marker byte, payload string) []byte {
	seg := []byte{0xff, marker}
	seg = binary.BigEndian.AppendUint16(seg, uint16((len(payload)+2)&0xffff))
	return append(seg, payload...)
}

// gpsExif is an Exif payload (big-endian) with orientation o, a pointer to a GPS directory, and in
// it a coordinate string kept out of line, as a camera writes them.
func gpsExif(o int) string {
	var b bytes.Buffer
	put16 := func(v int) { _ = binary.Write(&b, binary.BigEndian, uint16(v&0xffff)) }
	put32 := func(v int) { _ = binary.Write(&b, binary.BigEndian, uint32(v&0xffffffff)) }
	b.WriteString("MM\x00\x2a")
	put32(8)
	// IFD0 at 8: two entries (2 + 2*12 + 4 = 30 bytes), the GPS directory after it at 38.
	put16(2)
	put16(0x0112)
	put16(3)
	put32(1)
	put16(o)
	put16(0)
	put16(0x8825)
	put16(4)
	put32(1)
	put32(38)
	put32(0)
	// GPS directory at 38: one entry, the string at 38 + 2 + 12 + 4 = 56.
	put16(1)
	put16(0x0002)
	put16(2)
	put32(len("SECRET-GPS-COORDS") + 1)
	put32(56)
	put32(0)
	b.WriteString("SECRET-GPS-COORDS\x00")
	return "Exif\x00\x00" + b.String()
}

// photoWithMetadata is a JPEG with every kind of segment a camera or an editor adds, in the order
// they come: JFIF, Exif with a GPS directory and orientation o, XMP, a colour profile, Photoshop's
// IPTC, a vendor's APP15 and a comment.
func photoWithMetadata(t *testing.T, o int) []byte {
	t.Helper()
	plain := testJPEG(t, 64, 48)
	var segs []byte
	for _, seg := range [][]byte{
		jpegSegment(0xe0, "JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00"),
		jpegSegment(0xe1, gpsExif(o)),
		jpegSegment(0xe1, "http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta>SECRET-XMP</x:xmpmeta>"),
		jpegSegment(0xe2, "ICC_PROFILE\x00\x01\x01profile-bytes"),
		jpegSegment(0xed, "Photoshop 3.0\x008BIMSECRET-IPTC"),
		jpegSegment(0xef, "SECRET-APP"),
		jpegSegment(0xfe, "SECRET-COMMENT"),
	} {
		segs = append(segs, seg...)
	}
	return append(append(append([]byte{}, plain[:2]...), segs...), plain[2:]...)
}

// segments lists the markers of a JPEG up to its scan.
func segments(t *testing.T, data []byte) []byte {
	t.Helper()
	var markers []byte
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xff {
			t.Fatalf("not a marker at %d", i)
		}
		markers = append(markers, data[i+1])
		if data[i+1] == 0xda {
			break
		}
		i += 2 + int(binary.BigEndian.Uint16(data[i+2:]))
	}
	return markers
}

func decodeAll(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// stored is the size and received count the store has of an attachment.
func (w *world) stored(id string) (size, received int64) {
	w.e.t.Helper()
	if err := w.e.db.QueryRow(`SELECT size, received FROM app_attachments WHERE id = ?`, id).Scan(&size, &received); err != nil {
		w.e.t.Fatal(err)
	}
	return size, received
}

func TestJPEGMetadataIsStrippedAndOrientationKept(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	original := photoWithMetadata(t, 6)
	if len(original) > 200000 {
		t.Fatal("test photo too large")
	}

	id := w.upload(w.alice, s, a, "photos", original, 700)
	rec := w.fetch(w.alice, s, a, id, false)
	expect(t, rec, http.StatusOK)
	served := rec.Body.Bytes()

	noSecrets(t, "the photo", served)
	// JFIF and the colour profile stay; the only Exif left is one segment holding the orientation.
	if got, want := segments(t, served), []byte{0xe0, 0xe1, 0xe2, 0xdb}; !bytes.HasPrefix(got, want) {
		t.Fatalf("segments %x, want them to start %x", got, want)
	}
	if !bytes.Contains(served, []byte("ICC_PROFILE")) || !bytes.Contains(served, []byte("JFIF")) {
		t.Fatal("the JFIF header or the colour profile was dropped")
	}
	if n := bytes.Count(served, []byte("Exif\x00\x00")); n != 1 {
		t.Fatalf("%d Exif segments", n)
	}
	i := bytes.Index(served, []byte("\xff\xe1"))
	if size := binary.BigEndian.Uint16(served[i+2:]); size != 34 {
		t.Fatalf("the Exif segment is %d bytes, want 34: orientation only", size)
	}
	if got := orientationOf(served); got != 6 {
		t.Fatalf("orientation %d, want 6", got)
	}
	// Same picture, to the pixel, with the same size shown (the orientation turns it).
	if !reflect.DeepEqual(decodeAll(t, served), decodeAll(t, original)) {
		t.Fatal("the pixels changed")
	}
	got := decode[Attachment](t, w.alice.do(http.MethodGet, appPath(s, a.ID, "/uploads/"+id), nil))
	if got.Status != "ready" || got.Width != 48 || got.Height != 64 {
		t.Fatalf("attachment %+v: a photo turned by its orientation is shown 48 by 64", got)
	}
	// The thumbnail is turned upright, as before.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(w.fetch(w.alice, s, a, id, true).Body.Bytes()))
	if err != nil || cfg.Width != 48 || cfg.Height != 64 {
		t.Fatalf("thumbnail %v %v", cfg, err)
	}
}

func TestSizeIsWhatIsServed(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	original := photoWithMetadata(t, 1)

	id := w.start(w.alice, s, a, "photos", len(original))
	w.send(w.alice, s, a, id, original, 5000)
	rec := w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+id+"/finalize"), map[string]any{})
	expect(t, rec, http.StatusOK)
	ready := decode[Attachment](t, rec)
	served := w.fetch(w.alice, s, a, id, false).Body.Len()
	if served >= len(original) || ready.Size != int64(served) || ready.Received != ready.Size {
		t.Fatalf("original %d bytes, served %d, the answer says size %d, received %d", len(original), served, ready.Size, ready.Received)
	}
	if size, received := w.stored(id); size != int64(served) || received != size {
		t.Fatalf("the store has size %d, received %d; served %d", size, received, served)
	}
	// Orientation 1 needs no Exif at all.
	if bytes.Contains(w.fetch(w.alice, s, a, id, false).Body.Bytes(), []byte("Exif")) {
		t.Fatal("an upright photo keeps an Exif segment")
	}
	// The files are the stored picture and its thumbnail, and nothing of the upload.
	if files := w.files(s, a); len(files) != 2 {
		t.Fatalf("files = %v", files)
	}
	// And the agent is told the stored size.
	items := w.recordWith(w.alice, s, a, id)
	_ = items
	rows, err := w.e.s.ListRows(context.Background(), Actor{UserID: w.alice.id, Agent: true}, s.ID, a.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListRows = %v %v", rows, err)
	}
	if d, _ := rows[0]["photos"].([]Described); len(d) != 1 || d[0].Size != int64(served) {
		t.Fatalf("agent sees %+v", rows[0]["photos"])
	}
}

// pngChunk is a PNG chunk with its checksum.
func pngChunk(kind, data string) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(data)&0xffffffff))
	b = append(b, kind...)
	b = append(b, data...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE([]byte(kind+data)))
}

func pngChunks(t *testing.T, data []byte) []string {
	t.Helper()
	var kinds []string
	for i := 8; i+12 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[i:]))
		kinds = append(kinds, string(data[i+4:i+8]))
		i += 12 + n
	}
	return kinds
}

func TestPNGMetadataIsStripped(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	plain := testPNG(t, 40, 30)
	// After the header (8 + 25 bytes): metadata, and chunks that tell how to show the picture.
	var extra []byte
	for _, c := range [][]byte{
		pngChunk("tEXt", "Comment\x00SECRET-TEXT"),
		pngChunk("gAMA", "\x00\x00\xb1\x8f"),
		pngChunk("eXIf", gpsExif(1)[6:]),
		pngChunk("zTXt", "k\x00\x00SECRET-TEXT"),
		pngChunk("iTXt", "XML:com.adobe.xmp\x00\x00\x00\x00\x00SECRET-XMP"),
		pngChunk("tIME", "\x07\xea\x01\x02\x03\x04\x05"),
		pngChunk("pHYs", "\x00\x00\x0b\x13\x00\x00\x0b\x13\x01"),
	} {
		extra = append(extra, c...)
	}
	original := append(append(append([]byte{}, plain[:33]...), extra...), plain[33:]...)
	if got := pngChunks(t, original); !strings.Contains(strings.Join(got, " "), "eXIf") {
		t.Fatalf("test photo's chunks %v", got)
	}

	id := w.upload(w.alice, s, a, "photos", original, 4096)
	rec := w.fetch(w.alice, s, a, id, false)
	expect(t, rec, http.StatusOK)
	served := rec.Body.Bytes()

	noSecrets(t, "the photo", served)
	kinds := pngChunks(t, served)
	for _, dropped := range []string{"tEXt", "eXIf", "zTXt", "iTXt", "tIME"} {
		if strings.Contains(strings.Join(kinds, " "), dropped) {
			t.Errorf("%s was kept: %v", dropped, kinds)
		}
	}
	if len(kinds) < 4 || kinds[0] != "IHDR" || kinds[1] != "gAMA" || kinds[2] != "pHYs" || kinds[len(kinds)-1] != "IEND" {
		t.Fatalf("chunks %v", kinds)
	}
	// The kept chunks are the very bytes sent.
	if !bytes.Contains(served, pngChunk("pHYs", "\x00\x00\x0b\x13\x00\x00\x0b\x13\x01")) || !bytes.Contains(served, plain[33:]) {
		t.Fatal("a kept chunk changed")
	}
	if !reflect.DeepEqual(decodeAll(t, served), decodeAll(t, original)) {
		t.Fatal("the pixels changed")
	}
}

func TestGIFMetadataIsStripped(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	plain := testGIF(t)
	at := 13
	if plain[10]&0x80 != 0 {
		at += 3 << (plain[10]&7 + 1)
	}
	app := func(name, data string) []byte {
		b := []byte{0x21, 0xff, 11}
		b = append(b, name...)
		return append(append(b, byte(len(data)&0xff)), append([]byte(data), 0)...)
	}
	extra := append([]byte{}, app("NETSCAPE2.0", "\x01\x00\x00")...)
	extra = append(extra, app("XMP DataXMP", "SECRET-XMP")...)
	extra = append(extra, 0x21, 0xfe, byte(len("SECRET-COMMENT")))
	extra = append(extra, "SECRET-COMMENT"...)
	extra = append(extra, 0)
	extra = append(extra, app("ICCRGBG1012", "SECRET-APP")...)
	original := append(append(append([]byte{}, plain[:at]...), extra...), plain[at:]...)
	decodeAll(t, original)

	id := w.upload(w.alice, s, a, "photos", original, 100)
	rec := w.fetch(w.alice, s, a, id, false)
	expect(t, rec, http.StatusOK)
	served := rec.Body.Bytes()
	noSecrets(t, "the photo", served)
	if !bytes.Contains(served, []byte("NETSCAPE2.0")) {
		t.Fatal("the loop control was dropped")
	}
	if want := append(append(append([]byte{}, plain[:at]...), app("NETSCAPE2.0", "\x01\x00\x00")...), plain[at:]...); !bytes.Equal(served, want) {
		t.Fatalf("served %x\nwant   %x", served, want)
	}
	if !reflect.DeepEqual(decodeAll(t, served), decodeAll(t, original)) {
		t.Fatal("the pixels changed")
	}
}

// What can't be walked with certainty is refused, not guessed at.
func TestStrippingRefusesWhatItCannotWalk(t *testing.T) {
	jpg := photoWithMetadata(t, 6)
	png := testPNG(t, 8, 8)
	gif := testGIF(t)
	cases := []struct {
		name, format string
		data         []byte
	}{
		{"jpeg without a scan", "jpeg", jpg[:bytes.Index(jpg, []byte("\xff\xda"))]},
		{"jpeg segment past the end", "jpeg", append(append([]byte{}, jpg[:2]...), 0xff, 0xe1, 0xff, 0xff, 'x')},
		{"jpeg segment of no length", "jpeg", append(append([]byte{}, jpg[:2]...), 0xff, 0xe1, 0, 1, 0xff, 0xda)},
		{"jpeg that starts wrong", "jpeg", append([]byte{0}, jpg...)},
		{"jpeg with bytes between segments", "jpeg", append(append([]byte{}, jpg[:2]...), 0, 0xff, 0xda)},
		{"png without its end", "png", png[:len(png)-12]},
		{"png chunk past the end", "png", append(append([]byte{}, png[:33]...), 0x7f, 0, 0, 0, 't', 'E', 'X', 't')},
		{"gif without its trailer", "gif", gif[:len(gif)-1]},
		{"gif with an extension it doesn't know", "gif", append(append(append([]byte{}, gif[:len(gif)-1]...), 0x21, 0x77, 0), 0x3b)},
		{"gif with an unknown block", "gif", append(append(append([]byte{}, gif[:len(gif)-1]...), 0x55), 0x3b)},
		{"gif application extension that names nothing", "gif", append(append(append([]byte{}, gif[:len(gif)-1]...), 0x21, 0xff, 2, 'a', 'b', 0), 0x3b)},
		{"gif extension that runs out", "gif", append(append([]byte{}, gif[:len(gif)-1]...), 0x21, 0xfe, 9, 'a')},
	}
	for _, c := range cases {
		if _, err := stripMetadata(c.data, c.format); !errors.Is(err, errNotPicture) {
			t.Errorf("%s: err = %v, want errNotPicture", c.name, err)
		}
	}
	if _, err := stripMetadata(png, "webp"); !errors.Is(err, errNotPicture) {
		t.Errorf("another format: err = %v", err)
	}
	// Whatever follows the end of a PNG or a GIF is not the picture's.
	for _, c := range []struct {
		format string
		data   []byte
	}{{"png", png}, {"gif", gif}} {
		out, err := stripMetadata(append(append([]byte{}, c.data...), "SECRET-TEXT"...), c.format)
		if err != nil || !bytes.Equal(out, c.data) {
			t.Errorf("%s with trailing bytes: %v, unchanged = %v", c.format, err, bytes.Equal(out, c.data))
		}
	}
}

// A picture whose structure can't be walked is refused when it is finalized, and removed.
func TestFinalizeRefusesAPictureItCannotStrip(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	// A GIF with an extension nothing defines: the standard library reads it, this doesn't walk it.
	gif := testGIF(t)
	odd := append(append(append([]byte{}, gif[:len(gif)-1]...), 0x21, 0x77, 0), 0x3b)
	if _, _, err := image.DecodeConfig(bytes.NewReader(odd)); err != nil {
		t.Skipf("the image package doesn't read it: %v", err)
	}
	id := w.start(w.alice, s, a, "photos", len(odd))
	w.send(w.alice, s, a, id, odd, 100)
	expectCode(t, w.alice.do(http.MethodPost, appPath(s, a.ID, "/uploads/"+id+"/finalize"), map[string]any{}), http.StatusBadRequest, "app_attachment_type")
	if files := w.files(s, a); len(files) != 0 {
		t.Fatalf("files = %v", files)
	}
}

// Finalizing is stopped after the stored picture was written and before the store heard of it:
// the received file is still there, so the retry starts from it and ends with the stripped bytes.
func TestFinalizeAfterACrashBeforeTheCommit(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	original := photoWithMetadata(t, 6)
	id := w.start(w.alice, s, a, "photos", len(original))
	w.send(w.alice, s, a, id, original, 5000)

	// What the stopped finalize left: a stored picture (here a stale one, which must be replaced)
	// and a thumbnail, with the row still "uploading" and the received file whole.
	dir := filepath.Join(s.Dir(w.e.dataDir), filesDir, fmt.Sprint(a.ID))
	if err := os.WriteFile(filepath.Join(dir, id), []byte("half-written stored picture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := decode[Attachment](t, w.alice.do(http.MethodGet, appPath(s, a.ID, "/uploads/"+id), nil)).Status; status != "uploading" {
		t.Fatalf("status %s", status)
	}
	// Not readable until the store says so.
	expect(t, w.fetch(w.alice, s, a, id, false), http.StatusNotFound)

	// A new server (the old one is gone) finalizes it again.
	restarted := New(w.e.db, w.e.s.access).WithFiles(w.e.dataDir)
	ready, err := restarted.finalizeUpload(context.Background(), s, Actor{UserID: w.alice.id}, a.ID, id)
	if err != nil || ready.Status != "ready" {
		t.Fatalf("finalize = %+v, %v", ready, err)
	}
	served := w.fetch(w.alice, s, a, id, false).Body.Bytes()
	noSecrets(t, "the photo", served)
	if orientationOf(served) != 6 || ready.Size != int64(len(served)) || len(served) >= len(original) {
		t.Fatalf("served %d bytes (orientation %d), original %d, size %d", len(served), orientationOf(served), len(original), ready.Size)
	}
	if !reflect.DeepEqual(decodeAll(t, served), decodeAll(t, original)) {
		t.Fatal("the pixels changed")
	}
	if files := w.files(s, a); len(files) != 2 {
		t.Fatalf("files = %v", files)
	}
	// Finalizing again answers the same, and a chunk is refused as before.
	again, err := restarted.finalizeUpload(context.Background(), s, Actor{UserID: w.alice.id}, a.ID, id)
	if err != nil || again != ready {
		t.Fatalf("finalize again = %+v, %v", again, err)
	}
}

// Finalizing is stopped after the commit and before the received file was removed: the original,
// with its metadata, is only a leftover, which the sweep removes and a move doesn't carry over.
func TestAReceivedFileLeftAfterReadyIsSwept(t *testing.T) {
	w := newWorld(t)
	s := w.alicePrivate
	a := w.createPhotos(w.alice, s, "Photos")
	original := photoWithMetadata(t, 6)
	id := w.upload(w.alice, s, a, "photos", original, 5000)
	left := filepath.Join(s.Dir(w.e.dataDir), filesDir, fmt.Sprint(a.ID), id+".part")
	if err := os.WriteFile(left, original, 0o600); err != nil {
		t.Fatal(err)
	}
	// What is served is still the stripped picture, and finalizing again changes nothing.
	noSecrets(t, "the photo", w.fetch(w.alice, s, a, id, false).Body.Bytes())

	// A sweep with a cutoff in the past (nothing is old) still removes it: it is of no use at all.
	if err := w.e.s.Sweep(context.Background(), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the leftover is still there: %v", err)
	}
	if files := w.files(s, a); len(files) != 2 {
		t.Fatalf("files = %v", files)
	}
	expect(t, w.fetch(w.alice, s, a, id, false), http.StatusOK)
}

// What follows a JPEG's end (the video of a phone's motion photo, with metadata of its own) is not
// the picture, and is not kept.
func TestJPEGTrailerAfterTheEndIsDropped(t *testing.T) {
	original := photoWithMetadata(t, 1)
	trailer := []byte("ftypmp42 GPS 48.8584N 2.2945E")
	stripped, err := stripJPEG(append(append([]byte{}, original...), trailer...))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stripped, []byte("GPS 48.8584N")) {
		t.Fatal("the data after the picture's end was kept")
	}
	if !bytes.HasSuffix(stripped, []byte{0xff, 0xd9}) {
		t.Fatal("the stripped picture doesn't end at its EOI")
	}
	if !reflect.DeepEqual(decodeAll(t, stripped), decodeAll(t, original)) {
		t.Fatal("the pixels changed")
	}
	// A picture that never ends is refused rather than guessed at.
	if _, err := stripJPEG(original[:len(original)-2]); !errors.Is(err, errNotPicture) {
		t.Fatalf("a JPEG without its end: %v", err)
	}
}
