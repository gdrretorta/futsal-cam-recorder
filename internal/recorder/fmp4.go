package recorder

import (
	"encoding/binary"
	"errors"
	"io"
)

// The live view is built from the fragmented MP4 that ffmpeg is writing
// (see live.go). This file reads that MP4: the init section (ftyp + moov)
// and, after it, one moof + mdat pair per fragment. With
// -movflags frag_keyframe every fragment starts on a video keyframe, so each
// one can be an HLS segment as it is, served straight from the file.

// maxBoxRead limits how much of a moov or moof box is read into memory.
const maxBoxRead = 16 << 20

// fragment is a complete moof + mdat range of the file, ready to serve.
type fragment struct {
	off, size int64
	dur       float64 // seconds of video
}

// fragScanner reads a growing fragmented MP4 incrementally: each scan
// continues where the previous one stopped, so a long recording isn't read
// again on every playlist request.
type fragScanner struct {
	off       int64 // next top-level box to read
	initEnd   int64 // end of the moov box; 0 until it has been written
	videoID   uint32
	timescale uint32
	trexDur   map[uint32]uint32 // track id → default sample duration

	start int64   // start of the fragments not yet returned; -1 if none
	dur   float64 // video duration of the moof read last
}

func newFragScanner() *fragScanner { return &fragScanner{start: -1} }

var errBadMP4 = errors.New("ficheiro de vídeo inválido")

// scan returns the fragments completed since the last scan. A fragment
// without video (audio only, rare) is joined to the next one, so every
// fragment starts with the moof of a video keyframe or right before it.
func (s *fragScanner) scan(f io.ReaderAt, size int64) ([]fragment, error) {
	var out []fragment
	for s.off+8 <= size {
		typ, hdr, boxSize, err := readBoxHeader(f, s.off, size)
		if err != nil {
			return out, err
		}
		if hdr == 0 || s.off+boxSize > size {
			break // still being written
		}
		switch typ {
		case "moov":
			body, err := readBody(f, s.off+hdr, boxSize-hdr)
			if err != nil {
				return out, err
			}
			s.parseMoov(body)
			s.initEnd = s.off + boxSize
		case "moof":
			if s.initEnd == 0 {
				return out, errBadMP4
			}
			body, err := readBody(f, s.off+hdr, boxSize-hdr)
			if err != nil {
				return out, err
			}
			if s.start < 0 {
				s.start = s.off
			}
			s.dur += s.moofDuration(body)
		case "mdat":
			if s.start >= 0 && s.dur > 0 {
				out = append(out, fragment{off: s.start, size: s.off + boxSize - s.start, dur: s.dur})
				s.start, s.dur = -1, 0
			}
		}
		s.off += boxSize
	}
	return out, nil
}

// readBoxHeader returns the type, header length and total size of the box at
// off. hdr is 0 when the header itself isn't complete yet.
func readBoxHeader(f io.ReaderAt, off, end int64) (typ string, hdr, size int64, err error) {
	var b [16]byte
	if _, err := f.ReadAt(b[:8], off); err != nil {
		return "", 0, 0, err
	}
	size, hdr, typ = int64(binary.BigEndian.Uint32(b[:4])), 8, string(b[4:8])
	switch size {
	case 0:
		// "Up to the end of the file": a box still being written.
		return typ, 0, 0, nil
	case 1:
		if off+16 > end {
			return typ, 0, 0, nil
		}
		if _, err := f.ReadAt(b[8:16], off+8); err != nil {
			return "", 0, 0, err
		}
		size, hdr = int64(binary.BigEndian.Uint64(b[8:16])), 16
	}
	if size < hdr {
		return "", 0, 0, errBadMP4
	}
	return typ, hdr, size, nil
}

func readBody(f io.ReaderAt, off, n int64) ([]byte, error) {
	if n > maxBoxRead {
		return nil, errBadMP4
	}
	b := make([]byte, n)
	_, err := f.ReadAt(b, off)
	return b, err
}

// eachBox calls fn for every complete child box in b.
func eachBox(b []byte, fn func(typ string, body []byte)) {
	for len(b) >= 8 {
		size, hdr := uint64(binary.BigEndian.Uint32(b[:4])), uint64(8)
		if size == 1 {
			if len(b) < 16 {
				return
			}
			size, hdr = binary.BigEndian.Uint64(b[8:16]), 16
		} else if size == 0 {
			size = uint64(len(b))
		}
		if size < hdr || size > uint64(len(b)) {
			return
		}
		fn(string(b[4:8]), b[hdr:size])
		b = b[size:]
	}
}

func u32(b []byte, off int) (uint32, bool) {
	if off < 0 || off+4 > len(b) {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[off:]), true
}

// parseMoov finds the video track, its timescale and the default sample
// durations (trex), which fragments may rely on.
func (s *fragScanner) parseMoov(b []byte) {
	s.trexDur = map[uint32]uint32{}
	eachBox(b, func(typ string, body []byte) {
		switch typ {
		case "trak":
			var id, scale uint32
			video := false
			eachBox(body, func(typ string, body []byte) {
				switch typ {
				case "tkhd":
					if len(body) > 0 && body[0] == 1 {
						id, _ = u32(body, 4+16)
					} else {
						id, _ = u32(body, 4+8)
					}
				case "mdia":
					eachBox(body, func(typ string, body []byte) {
						switch typ {
						case "mdhd":
							if len(body) > 0 && body[0] == 1 {
								scale, _ = u32(body, 4+16)
							} else {
								scale, _ = u32(body, 4+8)
							}
						case "hdlr":
							if len(body) >= 12 && string(body[8:12]) == "vide" {
								video = true
							}
						}
					})
				}
			})
			if video && s.timescale == 0 && scale > 0 {
				s.videoID, s.timescale = id, scale
			}
		case "mvex":
			eachBox(body, func(typ string, body []byte) {
				if typ == "trex" {
					id, ok1 := u32(body, 4)
					dur, ok2 := u32(body, 12)
					if ok1 && ok2 {
						s.trexDur[id] = dur
					}
				}
			})
		}
	})
}

// moofDuration sums the video sample durations of a moof, in seconds.
func (s *fragScanner) moofDuration(b []byte) float64 {
	if s.timescale == 0 {
		return 0
	}
	var total uint64
	eachBox(b, func(typ string, body []byte) {
		if typ != "traf" {
			return
		}
		var id, defDur uint32
		var sum uint64
		eachBox(body, func(typ string, body []byte) {
			switch typ {
			case "tfhd":
				flags, ok := u32(body, 0)
				if !ok {
					return
				}
				flags &= 0xffffff
				id, _ = u32(body, 4)
				off := 8
				if flags&0x01 != 0 {
					off += 8 // base data offset
				}
				if flags&0x02 != 0 {
					off += 4 // sample description index
				}
				if flags&0x08 != 0 {
					defDur, _ = u32(body, off)
				}
			case "trun":
				sum += trunDuration(body, &defDur, s.trexDur, &id)
			}
		})
		if id == s.videoID {
			total += sum
		}
	})
	return float64(total) / float64(s.timescale)
}

// trunDuration sums the sample durations of a trun box. tfhd comes before
// trun in a traf, so the track id and its default duration are known here.
func trunDuration(b []byte, defDur *uint32, trex map[uint32]uint32, id *uint32) uint64 {
	flags, ok := u32(b, 0)
	if !ok {
		return 0
	}
	flags &= 0xffffff
	count, ok := u32(b, 4)
	if !ok {
		return 0
	}
	off := 8
	if flags&0x01 != 0 {
		off += 4 // data offset
	}
	if flags&0x04 != 0 {
		off += 4 // first sample flags
	}
	if flags&0x100 == 0 {
		d := *defDur
		if d == 0 {
			d = trex[*id]
		}
		return uint64(count) * uint64(d)
	}
	entry := 0
	for _, bit := range []uint32{0x100, 0x200, 0x400, 0x800} {
		if flags&bit != 0 {
			entry += 4
		}
	}
	var sum uint64
	for i := 0; i < int(count); i++ {
		d, ok := u32(b, off+i*entry)
		if !ok {
			break
		}
		sum += uint64(d)
	}
	return sum
}
