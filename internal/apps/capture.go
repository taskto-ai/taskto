package apps

import (
	"encoding/json"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"taskto.ai/taskto/internal/apierr"
)

// Fields that capture something from the device or the person (definition version 2): a place, a
// signature, a scanned code.
//
// A location is where the person was, with how sure the device was (accuracy_m) and when it
// said so (captured_at). It is evidence: the browser asks for it once, on a button or when the
// form is submitted, and never follows the person around; the server only stores what a screen
// sends, validated strictly here. A signature is a PNG picture held as an attachment
// (attachments.go), whose id the record keeps. Both are locked once stored: no later update (a
// screen's, the records explorer's, any release's) can change or clear them, unless the field says
// "editable". A barcode is the text a scanner (or the person's typing) read, optionally of a
// stated format.

// The capture types of field.
const (
	// TypeLocation is {lat, lon, accuracy_m?, captured_at, source}.
	TypeLocation = "location"
	// TypeSignature is the id of a PNG attachment.
	TypeSignature = "signature"
	// TypeBarcode is the text of a barcode or QR code.
	TypeBarcode = "barcode"
)

// The capabilities a client needs to hold these fields: to read and send their values.
const (
	LocationCapability  = "field.location"
	SignatureCapability = "field.signature"
	BarcodeCapability   = "field.barcode"
)

// fieldCapabilities is the capability each kind of field needs of a client that draws an app with one.
var fieldCapabilities = map[string]string{
	TypeAttachment: AttachmentCapability,
	TypeLocation:   LocationCapability,
	TypeSignature:  SignatureCapability,
	TypeBarcode:    BarcodeCapability,
}

// When a location is taken: on the person's pressing a button (the default), or when they submit the
// form. Either way once and in the foreground.
const (
	CaptureButton = "button"
	CaptureSubmit = "submit"
)

// Where a location came from: the device's own position, or typed by the person when the device
// couldn't give it.
const (
	SourceDevice = "device"
	SourceManual = "manual"
)

// Bounds of captured values.
const (
	// MaxLocationAccuracy is the largest accuracy_m taken, in metres: a radius of 1,000 km is no position.
	MaxLocationAccuracy = 1_000_000
	// MaxClockSkew is how far ahead of the server's clock a location's captured_at may be: a phone's
	// clock is not the server's.
	MaxClockSkew = 5 * time.Minute
	// DefaultSignatureBytes is the size a signature's PNG may have when the field says nothing.
	DefaultSignatureBytes = 1 << 20
)

// earliestCapture is the oldest captured_at taken: nothing of this was captured before it.
var earliestCapture = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// capturedFormat is how captured_at is stored: UTC, in milliseconds.
const capturedFormat = "2006-01-02T15:04:05.000Z"

// BarcodeFormats are the formats a barcode field may name, as the browser's BarcodeDetector names them.
var BarcodeFormats = []string{"aztec", "codabar", "code_39", "code_93", "code_128", "data_matrix", "ean_8", "ean_13",
	"itf", "pdf417", "qr_code", "upc_a", "upc_e"}

// checkedFormats are the formats whose text is a number the server can check: only digits, of the
// format's length, and for most the check digit.
var checkedFormats = map[string]struct {
	lengths []int
	check   bool
}{
	"ean_13": {[]int{13}, true},
	"ean_8":  {[]int{8}, true},
	"upc_a":  {[]int{12}, true},
	"upc_e":  {[]int{6, 7, 8}, false},
}

// checkDigitOK reports whether digits end in the right GS1 check digit: the weights 3 and 1
// alternate from the digit before it.
func checkDigitOK(digits string) bool {
	sum := 0
	for i := len(digits) - 2; i >= 0; i-- {
		d := int(digits[i] - '0')
		if (len(digits)-2-i)%2 == 0 {
			d *= 3
		}
		sum += d
	}
	return (10-sum%10)%10 == int(digits[len(digits)-1]-'0')
}

// badBarcode is the error of a value that isn't the text of the field's code.
func (f Field) badBarcode() error {
	of := ""
	if f.Format != "" {
		of = " of format " + f.Format
	}
	return invalidAs(apierr.CodeAppFieldNotBarcode, apierr.Params{"field": f.Name, "format": f.Format}, "%s must be the text of a code%s", f.Name, of)
}

// parseBarcode reads a barcode field's value: text with no control characters, of at most MaxLength
// characters, valid for the field's format if it names one whose text can be checked.
func (f Field) parseBarcode(s string) (any, bool, error) {
	bad := f.badBarcode
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return nil, false, bad()
	}
	if utf8.RuneCountInString(s) > f.MaxLength {
		return nil, false, invalidAs(apierr.CodeAppFieldTooLong, apierr.Params{"field": f.Name, "max": f.MaxLength}, "%s is at most %d characters", f.Name, f.MaxLength)
	}
	if rule, ok := checkedFormats[f.Format]; ok {
		if !slices.Contains(rule.lengths, len(s)) || strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) >= 0 || (rule.check && !checkDigitOK(s)) {
			return nil, false, bad()
		}
	}
	return s, true, nil
}

// jsonNumber reads a JSON number only (no text that reads as one), and not NaN or infinity.
func jsonNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	case json.Number:
		f, err := n.Float64()
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	return 0, false
}

var locationKeys = []string{"lat", "lon", "accuracy_m", "captured_at", "source"}

// parseLocation reads a location field's value and returns it as it is stored: lat and lon in
// degrees within the globe, accuracy_m (metres, 0 to MaxLocationAccuracy; the device's own
// reading, so a manual position may leave it out), captured_at in UTC, and source. An object with
// no keys is no value. Whether captured_at is not in the future is checked unless the field is
// read as stored (Field.asStored): a row keeps what was true when it was captured.
func (f Field) parseLocation(v any) (any, bool, error) {
	bad := func(format string, args ...any) error {
		return invalidAs(apierr.CodeAppFieldNotLocation, apierr.Params{"field": f.Name}, format, args...)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, false, bad("%s must be a location: {lat, lon, accuracy_m, captured_at, source}", f.Name)
	}
	if len(obj) == 0 {
		return nil, false, nil
	}
	for k := range obj {
		if !slices.Contains(locationKeys, k) {
			return nil, false, bad("%s: a location has lat, lon, accuracy_m, captured_at and source, not %q", f.Name, k)
		}
	}
	lat, ok1 := jsonNumber(obj["lat"])
	lon, ok2 := jsonNumber(obj["lon"])
	if !ok1 || !ok2 || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return nil, false, bad("%s: lat is a number from -90 to 90 and lon from -180 to 180", f.Name)
	}
	source, _ := obj["source"].(string)
	if source != SourceDevice && source != SourceManual {
		return nil, false, bad("%s: source is %q or %q", f.Name, SourceDevice, SourceManual)
	}
	out := map[string]any{"lat": lat, "lon": lon, "source": source}
	if raw, given := obj["accuracy_m"]; given && raw != nil {
		acc, ok := jsonNumber(raw)
		if !ok || acc < 0 || acc > MaxLocationAccuracy {
			return nil, false, bad("%s: accuracy_m is a number of metres from 0 to %d", f.Name, MaxLocationAccuracy)
		}
		out["accuracy_m"] = acc
	} else if source == SourceDevice {
		return nil, false, bad("%s: a position from the device says its accuracy_m", f.Name)
	}
	text, _ := obj["captured_at"].(string)
	at, err := time.Parse(time.RFC3339, text)
	if err != nil || !validZone(text) || at.Before(earliestCapture) {
		return nil, false, bad("%s: captured_at is a time, RFC 3339, from 2000 on", f.Name)
	}
	if !f.asStored && at.After(time.Now().Add(MaxClockSkew)) {
		return nil, false, invalidAs(apierr.CodeAppFieldLocationFuture, apierr.Params{"field": f.Name},
			"%s: captured_at is later than this server's clock (more than %v of difference)", f.Name, MaxClockSkew)
	}
	out["captured_at"] = at.UTC().Format(capturedFormat)
	return out, true, nil
}

// zonePattern is the end of an RFC 3339 time: Z, or an offset of at most 23:59.
var zonePattern = regexp.MustCompile(`(Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$`)

// validZone reports whether the time's zone is one: Go's parser lets an offset of +24:60 through.
func validZone(text string) bool { return zonePattern.MatchString(text) }

// parseSignature reads a signature field's value: the id of one attachment (or a list of one).
// Whether it may be used, and that it is a PNG, is for claimAttachments.
//
// A row holds the id alone. A list of one is taken from a screen (what a picture input binds), but never
// as a stored value: that is what a field that was an attachment field held, a photo that is no
// signature, and it is hidden as any value of another type is.
func (f Field) parseSignature(v any) (any, bool, error) {
	bad := invalidAs(apierr.CodeAppFieldNotSignature, apierr.Params{"field": f.Name}, "%s must be the id of one signature picture", f.Name)
	if list, ok := v.([]any); ok && !f.asStored {
		if len(list) == 0 {
			return nil, false, nil
		}
		if len(list) != 1 {
			return nil, false, bad
		}
		v = list[0]
	}
	id, ok := v.(string)
	if !ok || !attachmentIDPattern.MatchString(id) {
		return nil, false, bad
	}
	return id, true, nil
}

// locked is what a field's stored value was, when it can no longer change: the field is evidence
// (a location or a signature) that doesn't say it is editable, and the row holds a value.
func (f Field) locked(row map[string]any) (any, bool) {
	if (f.Type != TypeLocation && f.Type != TypeSignature) || f.Editable {
		return nil, false
	}
	v, ok := row[f.Name]
	if !ok || v == nil {
		return nil, false
	}
	value, present, err := f.stored(v)
	return value, err == nil && present
}

// lockedChange refuses a change that would alter a captured location or a signature the row
// already holds (replacing or clearing it): once captured, it stays as it was. Setting one the row
// has none of is the capture, and is let through, as is giving it again unchanged.
func (c Collection) lockedChange(before, after map[string]any) error {
	for _, f := range c.Fields {
		had, locked := f.locked(before)
		if !locked {
			continue
		}
		now, present, err := f.stored(after[f.Name])
		if err == nil && present && reflect.DeepEqual(had, now) {
			continue
		}
		return invalidAs(apierr.CodeAppFieldLocked, apierr.Params{"field": f.Name},
			"%s was captured and can't be changed: only a field that says it is editable can", f.Name)
	}
	return nil
}
