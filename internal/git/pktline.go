package git

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// Pkt-line codec (04_git.md §5). One line = 4 hex length bytes (total length
// incl. the 4) + payload. Max total 65520 → payload ≤ 65516. Special lengths:
// 0000 flush-pkt, 0001 delim-pkt, 0002 response-end (v2).

const (
	MaxPayload  = 65516 // max payload bytes per pkt-line
	MaxPktTotal = 65520 // max total length incl. the 4 length bytes

	PktKindData   = 0 // regular data pkt
	PktKindFlush  = 1 // 0000
	PktKindDelim  = 2 // 0001
	PktKindResEnd = 3 // 0002 (v2 response-end)
)

var ErrPktProtocol = errors.New("pkt-line protocol error")

// Pkt encodes one data pkt-line: payload + "\n" when absent (callers pass
// exact payloads; Pkt only appends the newline the wire grammar implies).
func Pkt(s string) []byte {
	if !bytes.HasSuffix([]byte(s), []byte{'\n'}) {
		s += "\n"
	}
	return dataPkt([]byte(s))
}

// PktBytes encodes a data pkt-line from exact payload bytes (no newline added).
func PktBytes(p []byte) []byte { return dataPkt(p) }

func dataPkt(p []byte) []byte {
	if len(p) > MaxPayload {
		// Callers must chunk; encode defensively by truncating is wrong — panic
		// is acceptable only for programmer error, so return an oversized-noop
		// via error contract instead. We choose: chunk callers use EncodeBands.
		panic(fmt.Sprintf("pkt payload %d exceeds %d", len(p), MaxPayload))
	}
	n := len(p) + 4
	out := make([]byte, n)
	out[0] = hexDigit(n >> 12)
	out[1] = hexDigit(n >> 8)
	out[2] = hexDigit(n >> 4)
	out[3] = hexDigit(n)
	copy(out[4:], p)
	return out
}

func hexDigit(v int) byte { return "0123456789abcdef"[v&0xf] }

// Flush returns the flush-pkt bytes.
func Flush() []byte { return []byte("0000") }

// Delim returns the delim-pkt bytes (v2).
func Delim() []byte { return []byte("0001") }

// ResponseEnd returns the v2 response-end pkt bytes.
func ResponseEnd() []byte { return []byte("0002") }

// PktReader decodes a pkt-line stream (04_git.md §5 decoder).
type PktReader struct {
	r   io.Reader
	buf [4]byte
}

func NewPktReader(r io.Reader) *PktReader { return &PktReader{r: r} }

// Next returns the payload and kind of the next pkt. At a flush/delim/
// response-end the payload is nil. A non-hex length or a payload longer than
// 65516 is a protocol error (ErrPktProtocol) → callers reject with pkt ERR.
// io.EOF is returned only at a clean end-of-stream.
func (pr *PktReader) Next() ([]byte, int, error) {
	if _, err := io.ReadFull(pr.r, pr.buf[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return nil, 0, fmt.Errorf("%w: truncated length header", ErrPktProtocol)
		}
		return nil, 0, err // io.EOF or reader error
	}
	n, ok := parseHex4(pr.buf[:])
	if !ok {
		return nil, 0, fmt.Errorf("%w: non-hex length %q", ErrPktProtocol, string(pr.buf[:]))
	}
	switch n {
	case 0:
		return nil, PktKindFlush, nil
	case 1:
		return nil, PktKindDelim, nil
	case 2:
		return nil, PktKindResEnd, nil
	case 3:
		return nil, 0, fmt.Errorf("%w: invalid length 0003", ErrPktProtocol)
	}
	if n > MaxPktTotal {
		return nil, 0, fmt.Errorf("%w: length %d exceeds %d", ErrPktProtocol, n, MaxPktTotal)
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(pr.r, payload); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, 0, fmt.Errorf("%w: truncated pkt payload", ErrPktProtocol)
		}
		return nil, 0, err
	}
	return payload, PktKindData, nil
}

func parseHex4(b []byte) (int, bool) {
	n := 0
	for _, c := range b {
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		default:
			return 0, false
		}
		n = n<<4 | v
	}
	return n, true
}

// ReadAllPkts drains a pkt stream into payloads until flush or EOF. Data after
// the first flush (pack bytes) must be handled by the caller with RawPkts.
func ReadAllPkts(r io.Reader) ([][]byte, error) {
	var out [][]byte
	pr := NewPktReader(r)
	for {
		p, kind, err := pr.Next()
		if err != nil {
			if err == io.EOF {
				return out, nil
			}
			return out, err
		}
		if kind == PktKindFlush {
			return out, nil
		}
		if kind != PktKindData {
			continue
		}
		out = append(out, p)
	}
}

// EncodeSideband chunks payload into band frames (≤ 65516-byte payloads) for
// the given band (1 = data, 2 = progress/message, 3 = fatal). An empty payload
// emits nothing (04_git.md §7.2: an empty band-1 frame is NOT sent).
func EncodeSideband(band byte, payload []byte) []byte {
	var out bytes.Buffer
	for len(payload) > 0 {
		chunk := payload
		if len(chunk) > MaxPayload-1 { // band byte + payload must fit 65520-4
			chunk = chunk[:MaxPayload-1]
		}
		n := len(chunk) + 5
		out.WriteByte(hexDigit(n >> 12))
		out.WriteByte(hexDigit(n >> 8))
		out.WriteByte(hexDigit(n >> 4))
		out.WriteByte(hexDigit(n))
		out.WriteByte(band)
		out.Write(chunk)
		payload = payload[len(chunk):]
	}
	return out.Bytes()
}

// SidebandDecode unwraps band frames: returns the concatenated band-1 payload
// plus all band-2/band-3 messages in order.
func SidebandDecode(data []byte) (payload, messages []byte, err error) {
	pr := NewPktReader(bytes.NewReader(data))
	for {
		p, kind, err := pr.Next()
		if err != nil {
			if err == io.EOF {
				return payload, messages, nil
			}
			return payload, messages, err
		}
		if kind == PktKindFlush {
			return payload, messages, nil
		}
		if kind != PktKindData || len(p) == 0 {
			continue
		}
		switch p[0] {
		case 1:
			payload = append(payload, p[1:]...)
		case 2, 3:
			messages = append(messages, p[1:]...)
		default:
			return payload, messages, fmt.Errorf("%w: bad sideband band %d", ErrPktProtocol, p[0])
		}
	}
}

// FirstNul splits the v0 first line: capabilities embed after a single NUL
// byte (04_git.md §5 — split on the first \x00 only).
func FirstNul(line []byte) (before, after []byte) {
	if i := bytes.IndexByte(line, 0); i >= 0 {
		return line[:i], line[i+1:]
	}
	return line, nil
}
