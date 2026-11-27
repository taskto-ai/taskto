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

const (
	TypeLocation  = "location"
	TypeSignature = "signature"
	TypeBarcode   = "barcode"
)

const (
	LocationCapability  = "field.location"
	SignatureCapability = "field.signature"
	BarcodeCapability   = "field.barcode"
)

var fieldCapabilities = map[string]string{
	TypeAttachment: AttachmentCapability,
	TypeLocation:   LocationCapability,
	TypeSignature:  SignatureCapability,
	TypeBarcode:    BarcodeCapability,
}

const (
	CaptureButton = "button"
	CaptureSubmit = "submit"
)

const (
	SourceDevice = "device"
	SourceManual = "manual"
)

const (
	MaxLocationAccuracy   = 1_000_000
	MaxClockSkew          = 5 * time.Minute
	DefaultSignatureBytes = 1 << 20
)

var earliestCapture = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

const capturedFormat = "2006-01-02T15:04:05.000Z"

var BarcodeFormats = []string{"aztec", "codabar", "code_39", "code_93", "code_128", "data_matrix", "ean_8", "ean_13",
	"itf", "pdf417", "qr_code", "upc_a", "upc_e"}

var checkedFormats = map[string]struct {
	lengths []int
	check   bool
}{
	"ean_13": {[]int{13}, true},
	"ean_8":  {[]int{8}, true},
	"upc_a":  {[]int{12}, true},
	"upc_e":  {[]int{6, 7, 8}, false},
}

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

func (f Field) badBarcode() error {
	of := ""
	if f.Format != "" {
		of = " of format " + f.Format
	}
	return invalidAs(apierr.CodeAppFieldNotBarcode, apierr.Params{"field": f.Name, "format": f.Format}, "%s must be the text of a code%s", f.Name, of)
}

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

var zonePattern = regexp.MustCompile(`(Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$`)

func validZone(text string) bool { return zonePattern.MatchString(text) }

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
