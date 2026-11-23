package apps

import (
	"bytes"
	"encoding/binary"
)

// Metadata. A photo's bytes are served to every reader of the record that holds it, so what a
// camera wrote into them (where it was taken, when, with what, by whom, a thumbnail of the
// uncropped original) is removed when the upload is finalized. It is removed without decoding or
// re-encoding anything: the segments, chunks and blocks that only carry metadata are left out and
// all the rest is copied as it is, so the picture's pixels are exactly what was sent. A file whose
// structure can't be walked with certainty is refused (errNotPicture) rather than guessed at.

// stripMetadata returns data, a picture of the given format (as inspect found it), without its
// metadata.
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

// stripJPEG walks a JPEG's segments from its start up to the start of its scan (SOS), leaving out
// the application segments that hold metadata (APP1, where Exif and XMP live, APP3 to APP13, APP15)
// and comments, and keeping the rest (APP0's JFIF header, APP2's colour profile, APP14's Adobe
// transform, and every non-application segment). The scans are copied as they are, with the tables
// between them (a progressive picture's), up to the end of the picture (EOI): what follows it, such
// as the video of a phone's motion photo with its own metadata, is not the picture and is left out.
//
// The one thing of the metadata a browser needs to show the picture right is its orientation: if
// it isn't 1, a minimal Exif segment holding it alone is put after the JFIF header.
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
		// A marker is 0xff and a code; any number of 0xff may fill before it.
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
		case marker == 0xda: // the first scan: from here on, the picture's data
			insert()
			return copyScans(out, data, i)
		case marker == 0x00 || marker == 0xd8 || marker == 0xd9 || (marker >= 0xd0 && marker <= 0xd7) || marker == 0x01:
			// Not a segment that can come before the scan, or one that has no length: not a
			// structure this walks with certainty.
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
		if marker != 0xe0 { // the orientation goes after the JFIF header, before anything else
			insert()
		}
		out = append(out, seg...)
	}
}

// copyScans copies a JPEG's scans, from the SOS segment at i, and the segments between them, up to
// and with its EOI marker: nothing after it. A metadata segment between scans is left out too.
func copyScans(out, data []byte, i int) ([]byte, error) {
	for {
		// i is at a marker that has a length (SOS or a table): copy the segment.
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
			// Entropy-coded data: 0xff is followed by 0x00 (a stuffed byte), a restart marker or more
			// 0xff fill; anything else is the next marker.
			start := i
			for {
				if i+1 >= len(data) {
					return nil, errNotPicture // no end of the picture
				}
				if data[i] == 0xff && data[i+1] != 0x00 && data[i+1] != 0xff && (data[i+1] < 0xd0 || data[i+1] > 0xd7) {
					break
				}
				i++
			}
			out = append(out, data[start:i]...)
		}
		// i is at a marker, after any fill.
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

// dropsJPEGSegment reports whether a segment with this marker carries metadata the server doesn't
// keep: APP1 (Exif, XMP), APP3 to APP13 (Meta, Photoshop's IRB, IPTC, …), APP15, and comments.
// APP0 (JFIF), APP2 (the colour profile) and APP14 (Adobe's colour transform) change how the
// picture is read or shown, and stay.
func dropsJPEGSegment(marker byte) bool {
	switch {
	case marker == 0xe1, marker >= 0xe3 && marker <= 0xed, marker == 0xef, marker == 0xfe:
		return true
	}
	return false
}

// appendOrientation appends an APP1 Exif segment that holds only the orientation tag (big-endian):
// the header, one directory of one entry (tag 0x0112, SHORT, count 1), and no next directory.
func appendOrientation(out []byte, orientation int) []byte {
	var seg bytes.Buffer
	seg.Write([]byte{0xff, 0xe1, 0, 34}) // the length counts itself and the 32 bytes after it
	seg.WriteString("Exif\x00\x00")
	seg.WriteString("MM\x00\x2a")
	_ = binary.Write(&seg, binary.BigEndian, uint32(8)) // the directory follows the header
	_ = binary.Write(&seg, binary.BigEndian, uint16(1)) // one entry
	_ = binary.Write(&seg, binary.BigEndian, uint16(0x0112))
	_ = binary.Write(&seg, binary.BigEndian, uint16(3)) // SHORT
	_ = binary.Write(&seg, binary.BigEndian, uint32(1))
	_ = binary.Write(&seg, binary.BigEndian, uint16(orientation&0xffff))
	_ = binary.Write(&seg, binary.BigEndian, uint16(0)) // the value's padding
	_ = binary.Write(&seg, binary.BigEndian, uint32(0)) // no next directory
	return append(out, seg.Bytes()...)
}

// stripPNG walks a PNG's chunks and leaves out the ones that hold metadata: eXIf, the text chunks
// (tEXt, zTXt, iTXt, where XMP is also kept) and tIME. The chunks it keeps are copied as they are,
// checksums included. Whatever follows the last chunk (IEND) is not part of the picture, and is
// not kept.
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
		end := i + 12 + int(length) // length, type, data, checksum
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

// stripGIF walks a GIF's blocks and leaves out comment extensions and the application extensions
// other than the two that tell a viewer an animation loops (NETSCAPE2.0 and ANIMEXTS1.0): that is
// where XMP and other makers' data are kept. The rest is copied as it is. A block this can't place
// with certainty (an extension it doesn't know, a length that runs out) is refused. Whatever
// follows the trailer is not part of the picture, and is not kept.
func stripGIF(data []byte) ([]byte, error) {
	if len(data) < 13 || (string(data[:6]) != "GIF87a" && string(data[:6]) != "GIF89a") {
		return nil, errNotPicture
	}
	// The header, the logical screen and its colour table, if it has one.
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
		case 0x3b: // the trailer
			return append(out, 0x3b), nil
		case 0x2c: // an image: its descriptor, its colour table if it has one, its data
			start := i
			if i+10 > len(data) {
				return nil, errNotPicture
			}
			packed := data[i+9]
			i += 10
			if packed&0x80 != 0 {
				i += 3 << (packed&7 + 1)
			}
			i++ // the minimum code size
			if i > len(data) {
				return nil, errNotPicture
			}
			end, ok := skipSubBlocks(data, i)
			if !ok {
				return nil, errNotPicture
			}
			out = append(out, data[start:end]...)
			i = end
		case 0x21: // an extension: its label, then sub-blocks
			if i+2 > len(data) {
				return nil, errNotPicture
			}
			start, label := i, data[i+1]
			end, ok := skipSubBlocks(data, i+2)
			if !ok {
				return nil, errNotPicture
			}
			switch label {
			case 0xf9, 0x01: // graphic control (transparency, delay), plain text
			case 0xfe: // comment
				i = end
				continue
			case 0xff: // application: its first sub-block is the 11 bytes naming it
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

// skipSubBlocks returns where the sub-blocks that start at i end: each is a length byte and that
// many bytes, up to one of length 0, which is included.
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
