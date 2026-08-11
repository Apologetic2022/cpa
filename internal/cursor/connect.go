package cursor

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/andybalholm/brotli"
)

const (
	flagCompressed      = 0x01
	flagEndStream       = 0x02
	connectGzipMinBytes = 1024
	maxConnectMessage   = 32 * 1024 * 1024
)

// EncodeEnvelope builds one Connect envelope without compressing the payload.
//
// Cursor Agent Run rejects compressed client envelopes unless the peer has
// explicitly negotiated connect-content-encoding (desktop/cursor2api leave
// outbound Agent frames uncompressed by default).
func EncodeEnvelope(payload []byte, endStream bool) []byte {
	return EncodeEnvelopeMaybeCompress(payload, endStream, false)
}

// EncodeEnvelopeMaybeCompress builds one Connect envelope, optionally gzip-
// compressing payloads larger than connectGzipMinBytes.
func EncodeEnvelopeMaybeCompress(payload []byte, endStream, compress bool) []byte {
	flags := byte(0)
	if endStream {
		flags |= flagEndStream
	}
	body := payload
	if compress && !endStream && len(payload) > connectGzipMinBytes {
		var buf bytes.Buffer
		zw, err := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
		if err == nil {
			if _, err = zw.Write(payload); err == nil {
				if err = zw.Close(); err == nil {
					body = buf.Bytes()
					flags |= flagCompressed
				}
			}
		}
	}
	out := make([]byte, 5+len(body))
	out[0] = flags
	binary.BigEndian.PutUint32(out[1:5], uint32(len(body)))
	copy(out[5:], body)
	return out
}

// SetCompression configures how compressed inbound envelopes are decoded.
func (d *Decoder) SetCompression(encoding string) {
	if d == nil {
		return
	}
	encoding = stringsToLower(encoding)
	if encoding == "" {
		encoding = "gzip"
	}
	d.compression = encoding
}

// Envelope is one decoded Connect frame.
type Envelope struct {
	Flags   byte
	Payload []byte
}

// Compressed reports whether the envelope payload is compressed.
func (e Envelope) Compressed() bool { return e.Flags&flagCompressed != 0 }

// EndStream reports whether this is a Connect end-stream trailer frame.
func (e Envelope) EndStream() bool { return e.Flags&flagEndStream != 0 }

// Decoder incrementally parses Connect envelopes from an HTTP/2 body stream.
type Decoder struct {
	buf         []byte
	compression string
}

// NewDecoder creates a Connect envelope decoder.
func NewDecoder() *Decoder {
	return &Decoder{compression: "gzip"}
}

// Feed appends bytes and returns complete envelopes.
func (d *Decoder) Feed(data []byte) ([]Envelope, error) {
	d.buf = append(d.buf, data...)
	var out []Envelope
	for len(d.buf) >= 5 {
		flags := d.buf[0]
		if flags&^(flagCompressed|flagEndStream) != 0 {
			return nil, fmt.Errorf("connect: reserved flags 0x%02x", flags)
		}
		length := int(binary.BigEndian.Uint32(d.buf[1:5]))
		if length > maxConnectMessage {
			return nil, fmt.Errorf("connect: message length %d exceeds limit", length)
		}
		frameEnd := 5 + length
		if len(d.buf) < frameEnd {
			break
		}
		payload := append([]byte(nil), d.buf[5:frameEnd]...)
		d.buf = d.buf[frameEnd:]
		if flags&flagCompressed != 0 {
			decoded, err := decompressPayload(payload, d.compression)
			if err != nil {
				return nil, err
			}
			payload = decoded
			flags &^= flagCompressed
		}
		out = append(out, Envelope{Flags: flags, Payload: payload})
	}
	return out, nil
}

func decompressPayload(payload []byte, encoding string) ([]byte, error) {
	switch stringsToLower(encoding) {
	case "", "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		return io.ReadAll(io.LimitReader(zr, maxConnectMessage+1))
	case "br":
		return io.ReadAll(io.LimitReader(brotli.NewReader(bytes.NewReader(payload)), maxConnectMessage+1))
	default:
		return nil, fmt.Errorf("connect: unsupported encoding %q", encoding)
	}
}

func stringsToLower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
