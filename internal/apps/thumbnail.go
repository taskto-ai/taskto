package apps

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"

	_ "image/gif"
	_ "image/png"
)

const ThumbSize = 320

var maxPixels = map[string]int{"jpeg": 50_000_000, "png": 24_000_000, "gif": 24_000_000}

var pictureTypes = map[string]struct{ contentType, ext string }{
	"jpeg": {"image/jpeg", "jpg"},
	"png":  {"image/png", "png"},
	"gif":  {"image/gif", "gif"},
}

type sniffed struct {
	format        string
	contentType   string
	width, height int
}

func inspect(data []byte) (sniffed, error) {
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	detected := map[string]string{"image/jpeg": "jpeg", "image/png": "png", "image/gif": "gif"}[http.DetectContentType(head)]
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || detected == "" || format != detected {
		return sniffed{}, errNotPicture
	}
	pt := pictureTypes[format]
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxPixels[format]/cfg.Height {
		return sniffed{}, errNotPicture
	}
	s := sniffed{format: format, contentType: pt.contentType, width: cfg.Width, height: cfg.Height}
	if format == "jpeg" && orientationOf(data) >= 5 {
		s.width, s.height = s.height, s.width
	}
	return s, nil
}

func thumbnail(data []byte, format string) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errNotPicture
	}
	b := img.Bounds()
	scale := float64(ThumbSize) / float64(max(b.Dx(), b.Dy()))
	tw, th := b.Dx(), b.Dy()
	if scale < 1 {
		tw, th = max(1, int(float64(tw)*scale)), max(1, int(float64(th)*scale))
	}
	small := image.NewRGBA(image.Rect(0, 0, tw, th))
	const grid = 4
	for y := range th {
		y0, y1 := b.Min.Y+y*b.Dy()/th, b.Min.Y+(y+1)*b.Dy()/th
		for x := range tw {
			x0, x1 := b.Min.X+x*b.Dx()/tw, b.Min.X+(x+1)*b.Dx()/tw
			var r, g, bl, a, n uint64
			nx, ny := min(grid, max(1, x1-x0)), min(grid, max(1, y1-y0))
			for sy := range ny {
				for sx := range nx {
					px := min(x0+sx*(x1-x0)/nx, b.Max.X-1)
					py := min(y0+sy*(y1-y0)/ny, b.Max.Y-1)
					cr, cg, cb, ca := img.At(px, py).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			white := 0xffff*n - a
			small.SetRGBA(x, y, color.RGBA{
				R: uint8((r + white) / n >> 8 & 0xff), G: uint8((g + white) / n >> 8 & 0xff), B: uint8((bl + white) / n >> 8 & 0xff), A: 0xff,
			})
		}
	}
	out := orient(small, orientationOfFormat(data, format))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func orientationOfFormat(data []byte, format string) int {
	if format == "jpeg" {
		return orientationOf(data)
	}
	return 1
}

func orient(img *image.RGBA, o int) image.Image {
	if o < 2 || o > 8 {
		return img
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range h {
		for x := range w {
			var nx, ny int
			switch o {
			case 2:
				nx, ny = w-1-x, y
			case 3:
				nx, ny = w-1-x, h-1-y
			case 4:
				nx, ny = x, h-1-y
			case 5:
				nx, ny = y, x
			case 6:
				nx, ny = h-1-y, x
			case 7:
				nx, ny = h-1-y, w-1-x
			case 8:
				nx, ny = y, w-1-x
			}
			out.SetRGBA(nx, ny, img.RGBAAt(x, y))
		}
	}
	return out
}

func orientationOf(data []byte) int {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 1
	}
	i := 2
	for n := 0; n < 32 && i+4 <= len(data); n++ {
		if data[i] != 0xff {
			return 1
		}
		marker := data[i+1]
		if marker == 0xda || marker == 0xd9 {
			return 1
		}
		size := int(binary.BigEndian.Uint16(data[i+2:]))
		if size < 2 || i+2+size > len(data) {
			return 1
		}
		if marker == 0xe1 {
			if o := tiffOrientation(data[i+4 : i+2+size]); o != 0 {
				return o
			}
		}
		i += 2 + size
	}
	return 1
}

func tiffOrientation(seg []byte) int {
	if !bytes.HasPrefix(seg, []byte("Exif\x00\x00")) || len(seg) < 6+8 {
		return 0
	}
	t := seg[6:]
	var order binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0
	}
	ifd := int(order.Uint32(t[4:]))
	if ifd < 8 || ifd+2 > len(t) {
		return 0
	}
	count := int(order.Uint16(t[ifd:]))
	for e := range min(count, 64) {
		at := ifd + 2 + e*12
		if at+12 > len(t) {
			return 0
		}
		if order.Uint16(t[at:]) == 0x0112 && order.Uint16(t[at+2:]) == 3 {
			if o := int(order.Uint16(t[at+8:])); o >= 1 && o <= 8 {
				return o
			}
			return 0
		}
	}
	return 0
}
