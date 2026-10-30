package shutterfly

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Date repair: JPEGs arriving from Shutterfly with malformed or missing EXIF
// dates land on their upload date in Google Photos. repairDateBytes returns
// corrected bytes (in-place tag patch or a fresh minimal EXIF APP1), the
// effective date, and where it came from.
//
// source is one of "exif-fixed", "filename", "neighbor", "unchanged".
func repairDateBytes(data []byte, filename string, neighbor time.Time) ([]byte, time.Time, string) {
	// Only JPEG: FF D8
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return data, time.Time{}, "unchanged"
	}

	app1Start, app1End, ifd0, _ := findExif(data)

	// (a) Patch out-of-range EXIF date tags in place.
	if ifd0 != nil {
		fixed, dt, changed := patchExifDates(data, ifd0)
		if changed {
			return fixed, dt, "exif-fixed"
		}
		// Valid DateTimeOriginal present: nothing to do.
		if !dt.IsZero() {
			return data, dt, "unchanged"
		}
	}

	// (b) No usable date: filename, then neighbor.
	dt := dateFromFilename(filename)
	src := "filename"
	if dt.IsZero() {
		if neighbor.IsZero() {
			return data, time.Time{}, "unchanged"
		}
		dt = neighbor
		src = "neighbor"
	}
	newBytes, err := writeExifDate(data, app1Start, app1End, dt)
	if err != nil {
		return data, time.Time{}, "unchanged"
	}
	return newBytes, dt, src
}

// ---------------------------------------------------------------------------
// EXIF APP1 location + IFD0 parse

type ifdEntry struct {
	tag   uint16
	typ   uint16
	count uint32
	// for count<=4 inline values; otherwise valueOff
	valueOff uint32
	inline   [4]byte
	entryPos int // offset of the 12-byte entry in data
}

type exifInfo struct {
	entries   []ifdEntry
	exifOff   int // offset of TIFF header ("MM"/"II")
	byteOrder binary.ByteOrder
	// absolute offsets of each date tag's ASCII value (19 bytes + NUL)
	dateTags map[uint16][]byte // tag -> raw current value
	datePos  map[uint16]int
	orient   uint16
	hasOrien bool
	exifIFD  *ifdEntry // pointer entry
}

func findExif(data []byte) (app1Start, app1End int, info *exifInfo, orient uint16) {
	pos := 2
	for pos+4 <= len(data) {
		if data[pos] != 0xFF {
			break
		}
		marker := data[pos+1]
		if marker == 0xD9 || marker == 0xDA { // EOI/SOS
			break
		}
		segLen := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
		end := pos + 2 + segLen
		if marker == 0xE1 && segLen >= 8 && bytes.Equal(data[pos+4:pos+10], []byte("Exif\x00\x00")) {
			info = parseTIFF(data, pos+10, end)
			return pos, end, info, 0
		}
		pos = end
	}
	return -1, -1, nil, 0
}

func parseTIFF(data []byte, tiffStart, limit int) *exifInfo {
	if tiffStart+8 > limit {
		return nil
	}
	var bo binary.ByteOrder
	switch string(data[tiffStart : tiffStart+2]) {
	case "MM":
		bo = binary.BigEndian
	case "II":
		bo = binary.LittleEndian
	default:
		return nil
	}
	info := &exifInfo{exifOff: tiffStart, byteOrder: bo, dateTags: map[uint16][]byte{}, datePos: map[uint16]int{}}
	ifdOff := int(bo.Uint32(data[tiffStart+4 : tiffStart+8]))
	ifd0pos := tiffStart + ifdOff
	info.entries = readIFD(data, tiffStart, ifd0pos, limit, bo)
	for _, e := range info.entries {
		switch e.tag {
		case 0x0112:
			info.orient = bo.Uint16(e.inline[:2])
			info.hasOrien = true
		case 0x8769:
			info.exifIFD = &e
		case 0x0132: // DateTime
			recordDateTag(info, data, tiffStart, e)
		}
	}
	if info.exifIFD != nil {
		sub := tiffStart + int(info.exifIFD.valueOff)
		for _, e := range readIFD(data, tiffStart, sub, limit, bo) {
			if e.tag == 0x9003 || e.tag == 0x9004 {
				recordDateTag(info, data, tiffStart, e)
			}
		}
	}
	return info
}

func recordDateTag(info *exifInfo, data []byte, tiffStart int, e ifdEntry) {
	if e.typ != 2 || e.count < 19 { // ASCII, at least "YYYY:MM:DD HH:MM:SS"
		return
	}
	abs := tiffStart + int(e.valueOff)
	if e.count <= 4 || abs+19 > len(data) {
		return
	}
	info.dateTags[e.tag] = data[abs : abs+19]
	info.datePos[e.tag] = abs
}

func readIFD(data []byte, tiffStart, pos, limit int, bo binary.ByteOrder) []ifdEntry {
	if pos+2 > limit {
		return nil
	}
	n := int(bo.Uint16(data[pos : pos+2]))
	var out []ifdEntry
	for i := 0; i < n; i++ {
		ep := pos + 2 + i*12
		if ep+12 > limit {
			break
		}
		e := ifdEntry{
			tag:      bo.Uint16(data[ep : ep+2]),
			typ:      bo.Uint16(data[ep+2 : ep+4]),
			count:    bo.Uint32(data[ep+4 : ep+8]),
			entryPos: ep,
		}
		if e.count <= 4 && (e.typ == 3 || e.typ == 8) {
			copy(e.inline[:], data[ep+8:ep+12])
		} else {
			e.valueOff = bo.Uint32(data[ep+8 : ep+12])
		}
		out = append(out, e)
	}
	return out
}

// patchExifDates normalizes each malformed "YYYY:MM:DD HH:MM:SS" in place via
// time.Date carry normalization. Returns modified copy, first valid date,
// and whether anything changed.
func patchExifDates(data []byte, info *exifInfo) ([]byte, time.Time, bool) {
	out := make([]byte, len(data))
	copy(out, data)
	var first time.Time
	changed := false
	for tag, raw := range info.dateTags {
		t, malformed := normalizeExifDate(string(raw))
		if malformed {
			// time.Date carries overflow (e.g. :75 -> +1:15)
			v := fmt.Sprintf("%04d:%02d:%02d %02d:%02d:%02d", t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second())
			copy(out[info.datePos[tag]:], v)
			changed = true
		}
		if first.IsZero() && tag == 0x9003 {
			first = t
		}
	}
	if first.IsZero() {
		for tag, raw := range info.dateTags {
			if t, _ := normalizeExifDate(string(raw)); !t.IsZero() && (tag == 0x0132 || tag == 0x9004) {
				first = t
				break
			}
		}
	}
	return out, first, changed
}

// normalizeExifDate parses EXIF datetime; malformed=false when every field is
// already in range (normalized equals input).
func normalizeExifDate(s string) (time.Time, bool) {
	s = strings.TrimRight(s, "\x00 ")
	var y, mo, d, h, mi, sec int
	if n, _ := fmt.Sscanf(s, "%d:%d:%d %d:%d:%d", &y, &mo, &d, &h, &mi, &sec); n != 6 {
		return time.Time{}, false
	}
	if y < 1 || mo < 0 || mo > 99 || d < 0 || h < 0 || mi < 0 || sec < 0 {
		return time.Time{}, false
	}
	inRange := mo >= 1 && mo <= 12 && d >= 1 && d <= 31 && h <= 23 && mi <= 59 && sec <= 59
	t := time.Date(y, time.Month(mo), d, h, mi, sec, 0, time.Local)
	return t, !inRange
}

// ---------------------------------------------------------------------------
// Filename dates

var filenameDatePatterns = []*regexp.Regexp{
	// 20150917_081955, IMG_20150917_081955, 20150917-081955 (+ -1, (0), ~2 suffixes)
	regexp.MustCompile(`(?:^|[^0-9])(\d{4})(\d{2})(\d{2})[_-](\d{2})(\d{2})(\d{2})(?:[^0-9]|$)`),
	// 2015-09-17_08.19.55 / 2015-09-17 08.19.55
	regexp.MustCompile(`(?:^|[^0-9])(\d{4})-(\d{2})-(\d{2})[_ ](\d{2})\.(\d{2})\.(\d{2})(?:[^0-9]|$)`),
	// Snapchat-<10 or 13 digit epoch>
	regexp.MustCompile(`Snapchat-(\d{10}|\d{13})(?:[^0-9]|$)`),
}

func dateFromFilename(name string) time.Time {
	for _, re := range filenameDatePatterns[:2] {
		m := re.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		h, _ := strconv.Atoi(m[4])
		mi, _ := strconv.Atoi(m[5])
		sec, _ := strconv.Atoi(m[6])
		if mo < 1 || mo > 12 || d < 1 || d > 31 || h > 23 || mi > 59 || sec > 59 {
			continue
		}
		return time.Date(y, time.Month(mo), d, h, mi, sec, 0, time.Local)
	}
	if m := filenameDatePatterns[2].FindStringSubmatch(name); m != nil {
		ep, _ := strconv.ParseInt(m[1], 10, 64)
		if len(m[1]) == 13 {
			ep /= 1000
		}
		t := time.Unix(ep, 0)
		if t.Year() >= 2010 && t.Year() <= 2025 {
			return t
		}
	}
	return time.Time{}
}

// ---------------------------------------------------------------------------
// Minimal EXIF writer

// writeExifDate replaces the Exif APP1 (or inserts one after SOI/APP0) with a
// minimal big-endian TIFF holding Orientation (if the original had one) and
// the three date tags set to dt.
func writeExifDate(data []byte, app1Start, app1End int, dt time.Time) ([]byte, error) {
	dateStr := fmt.Sprintf("%04d:%02d:%02d %02d:%02d:%02d", dt.Year(), dt.Month(), dt.Day(), dt.Hour(), dt.Minute(), dt.Second())

	var orient uint16
	hasOrient := false
	if app1Start >= 0 {
		if old := parseTIFF(data, app1Start+10, app1End); old != nil && old.hasOrien {
			orient = old.orient
			hasOrient = true
		}
	}

	// TIFF: header(8) + IFD0 + ExifIFD + data(3*20)
	ifd0Entries := 2 // DateTime + ExifIFD ptr
	if hasOrient {
		ifd0Entries++
	}
	ifd0Size := 2 + ifd0Entries*12 + 4
	exifIFDPos := 8 + ifd0Size
	exifIFDSize := 2 + 2*12 + 4
	dataPos := exifIFDPos + exifIFDSize

	buf := new(bytes.Buffer)
	buf.WriteString("MM")
	binary.Write(buf, binary.BigEndian, uint16(0x2A))
	binary.Write(buf, binary.BigEndian, uint32(8))

	// IFD0
	binary.Write(buf, binary.BigEndian, uint16(ifd0Entries))
	writeIFDEntry := func(tag, typ uint16, count, val uint32) {
		binary.Write(buf, binary.BigEndian, tag)
		binary.Write(buf, binary.BigEndian, typ)
		binary.Write(buf, binary.BigEndian, count)
		binary.Write(buf, binary.BigEndian, val)
	}
	if hasOrient {
		writeIFDEntry(0x0112, 3, 1, uint32(orient)<<16)
	}
	writeIFDEntry(0x0132, 2, 20, uint32(dataPos))
	writeIFDEntry(0x8769, 4, 1, uint32(exifIFDPos))
	binary.Write(buf, binary.BigEndian, uint32(0)) // next IFD

	// ExifIFD
	binary.Write(buf, binary.BigEndian, uint16(2))
	writeIFDEntry(0x9003, 2, 20, uint32(dataPos+20))
	writeIFDEntry(0x9004, 2, 20, uint32(dataPos+40))
	binary.Write(buf, binary.BigEndian, uint32(0))

	// data area
	for i := 0; i < 3; i++ {
		b := make([]byte, 20)
		copy(b, dateStr)
		buf.Write(b)
	}

	seg := new(bytes.Buffer)
	seg.Write([]byte{0xFF, 0xE1})
	binary.Write(seg, binary.BigEndian, uint16(buf.Len()+8)) // len includes 2 len bytes + "Exif\0\0"
	seg.WriteString("Exif\x00\x00")
	seg.Write(buf.Bytes())

	var out bytes.Buffer
	out.Write(data[:2])
	insertPos := 2
	// keep APP0 (FFE0) first if present
	if len(data) >= 4 && data[2] == 0xFF && data[3] == 0xE0 {
		l := int(binary.BigEndian.Uint16(data[4:6]))
		out.Write(data[2 : 2+2+l])
		insertPos = 2 + 2 + l
	}
	segBytes := seg.Bytes()
	out.Write(segBytes)
	if app1Start >= 0 {
		out.Write(data[insertPos:app1Start])
		out.Write(data[app1End:])
	} else {
		out.Write(data[insertPos:])
	}
	return out.Bytes(), nil
}
