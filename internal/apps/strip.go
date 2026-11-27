package apps

import (
	"bytes"
	"encoding/binary"
)

func stripMetadata(data []byte, format string) ([]byte, error) {
	switch format {
	case "jpeg":
		return stripJPEG(data)
	case "png":
		return stripPNG(data)
	case "gif":
		return stripGIF(data)
	}
	return nil, errNotPicture
}

func stripJPEG(data []byte) ([]byte, error) {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return nil, errNotPicture
	}
	orientation := orientationOf(data)
	out := make([]byte, 0, len(data))
	out = append(out, data[:2]...)
	inserted := orientation == 1
	insert := func() {
		if !inserted {
			out = appendOrientation(out, orientation)
			inserted = true
		}
	}
	i := 2
	for {
		if i >= len(data) || data[i] != 0xff {
			return nil, errNotPicture
		}
		for i+1 < len(data) && data[i+1] == 0xff {
			i++
		}
		if i+1 >= len(data) {
			return nil, errNotPicture
		}
		marker := data[i+1]
		switch {
		case marker == 0xda:
			insert()
			return copyScans(out, data, i)
		case marker == 0x00 || marker == 0xd8 || marker == 0xd9 || (marker >= 0xd0 && marker <= 0xd7) || marker == 0x01:
			return nil, errNotPicture
		}
		if i+4 > len(data) {
			return nil, errNotPicture
		}
		size := int(binary.BigEndian.Uint16(data[i+2:]))
		if size < 2 || i+2+size > len(data) {
			return nil, errNotPicture
		}
		seg := data[i : i+2+size]
		i += 2 + size
		if dropsJPEGSegment(marker) {
			continue
		}
		if marker != 0xe0 {
			insert()
		}
		out = append(out, seg...)
	}
}

func copyScans(out, data []byte, i int) ([]byte, error) {
	for {
		if i+4 > len(data) {
			return nil, errNotPicture
		}
		marker := data[i+1]
		size := int(binary.BigEndian.Uint16(data[i+2:]))
		if size < 2 || i+2+size > len(data) {
			return nil, errNotPicture
		}
		if !dropsJPEGSegment(marker) {
			out = append(out, data[i:i+2+size]...)
		}
		i += 2 + size
		if marker == 0xda {
			start := i
			for {
				if i+1 >= len(data) {
					return nil, errNotPicture
				}
				if data[i] == 0xff && data[i+1] != 0x00 && data[i+1] != 0xff && (data[i+1] < 0xd0 || data[i+1] > 0xd7) {
					break
				}
				i++
			}
			out = append(out, data[start:i]...)
		}
		for i+1 < len(data) && data[i] == 0xff && data[i+1] == 0xff {
			i++
		}
		if i+1 >= len(data) || data[i] != 0xff {
			return nil, errNotPicture
		}
		switch m := data[i+1]; {
		case m == 0xd9:
			return append(out, 0xff, 0xd9), nil
		case m == 0x00 || m == 0x01 || m == 0xd8 || (m >= 0xd0 && m <= 0xd7):
			return nil, errNotPicture
		}
	}
}

func dropsJPEGSegment(marker byte) bool {
	switch {
	case marker == 0xe1, marker >= 0xe3 && marker <= 0xed, marker == 0xef, marker == 0xfe:
		return true
	}
	return false
}

func appendOrientation(out []byte, orientation int) []byte {
	var seg bytes.Buffer
	seg.Write([]byte{0xff, 0xe1, 0, 34})
	seg.WriteString("Exif\x00\x00")
	seg.WriteString("MM\x00\x2a")
	_ = binary.Write(&seg, binary.BigEndian, uint32(8))
	_ = binary.Write(&seg, binary.BigEndian, uint16(1))
	_ = binary.Write(&seg, binary.BigEndian, uint16(0x0112))
	_ = binary.Write(&seg, binary.BigEndian, uint16(3))
	_ = binary.Write(&seg, binary.BigEndian, uint32(1))
	_ = binary.Write(&seg, binary.BigEndian, uint16(orientation&0xffff))
	_ = binary.Write(&seg, binary.BigEndian, uint16(0))
	_ = binary.Write(&seg, binary.BigEndian, uint32(0))
	return append(out, seg.Bytes()...)
}

func stripPNG(data []byte) ([]byte, error) {
	const signature = "\x89PNG\r\n\x1a\n"
	if !bytes.HasPrefix(data, []byte(signature)) {
		return nil, errNotPicture
	}
	out := make([]byte, 0, len(data))
	out = append(out, signature...)
	for i := len(signature); ; {
		if i+12 > len(data) {
			return nil, errNotPicture
		}
		length := binary.BigEndian.Uint32(data[i:])
		if length > 1<<31-1 || uint64(i)+12+uint64(length) > uint64(len(data)) {
			return nil, errNotPicture
		}
		end := i + 12 + int(length)
		kind := string(data[i+4 : i+8])
		switch kind {
		case "eXIf", "tEXt", "zTXt", "iTXt", "tIME":
		default:
			out = append(out, data[i:end]...)
		}
		i = end
		if kind == "IEND" {
			return out, nil
		}
	}
}

func stripGIF(data []byte) ([]byte, error) {
	if len(data) < 13 || (string(data[:6]) != "GIF87a" && string(data[:6]) != "GIF89a") {
		return nil, errNotPicture
	}
	i := 13
	if data[10]&0x80 != 0 {
		i += 3 << (data[10]&7 + 1)
	}
	if i > len(data) {
		return nil, errNotPicture
	}
	out := make([]byte, 0, len(data))
	out = append(out, data[:i]...)
	for {
		if i >= len(data) {
			return nil, errNotPicture
		}
		switch data[i] {
		case 0x3b:
			return append(out, 0x3b), nil
		case 0x2c:
			start := i
			if i+10 > len(data) {
				return nil, errNotPicture
			}
			packed := data[i+9]
			i += 10
			if packed&0x80 != 0 {
				i += 3 << (packed&7 + 1)
			}
			i++
			if i > len(data) {
				return nil, errNotPicture
			}
			end, ok := skipSubBlocks(data, i)
			if !ok {
				return nil, errNotPicture
			}
			out = append(out, data[start:end]...)
			i = end
		case 0x21:
			if i+2 > len(data) {
				return nil, errNotPicture
			}
			start, label := i, data[i+1]
			end, ok := skipSubBlocks(data, i+2)
			if !ok {
				return nil, errNotPicture
			}
			switch label {
			case 0xf9, 0x01:
			case 0xfe:
				i = end
				continue
			case 0xff:
				if i+14 > end || data[i+2] != 11 {
					return nil, errNotPicture
				}
				switch string(data[i+3 : i+14]) {
				case "NETSCAPE2.0", "ANIMEXTS1.0":
				default:
					i = end
					continue
				}
			default:
				return nil, errNotPicture
			}
			out = append(out, data[start:end]...)
			i = end
		default:
			return nil, errNotPicture
		}
	}
}

func skipSubBlocks(data []byte, i int) (int, bool) {
	for {
		if i >= len(data) {
			return 0, false
		}
		n := int(data[i])
		i += 1 + n
		if n == 0 {
			return i, i <= len(data)
		}
	}
}
