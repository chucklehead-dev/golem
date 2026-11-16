package bedrock

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// The AWS event stream's framing: a prelude (total length, headers'
// length, the prelude's CRC), the headers, the payload, the message's CRC,
// all integers big-endian and both CRCs CRC-32 (IEEE). It is read here
// rather than by the AWS SDK's decoder, which allocated a dozen times per
// message (its readers, a hash, every header's name and value).
const (
	preludeLen = 12
	minMessage = preludeLen + 4
	// maxMessage bounds one message, read whole into memory: the event
	// stream's own limit is 16 MiB of payload and 128 KiB of headers. The
	// prelude's CRC is checked before the length is trusted.
	maxMessage = 16<<20 + 128<<10 + minMessage
)

// errMalformed is a message whose framing or headers cannot be read.
var errMalformed = errors.New("a malformed event-stream message")

// message is one event-stream message's headers of interest and payload,
// each a view into the decoder's buffer, valid until the next read.
type message struct {
	messageType, eventType, exceptionType, errorCode []byte
	payload                                          []byte
}

// streamDecoder reads event-stream messages from src into one buffer it
// reuses.
type streamDecoder struct {
	prelude [preludeLen]byte
	buf     []byte // the message after its prelude
}

// next reads the next message. It returns io.EOF only at a message
// boundary; a stream cut within a message is io.ErrUnexpectedEOF.
func (d *streamDecoder) next(src io.Reader, m *message) error {
	if _, err := io.ReadFull(src, d.prelude[:]); err != nil {
		return err
	}
	total := binary.BigEndian.Uint32(d.prelude[0:4])
	headers := binary.BigEndian.Uint32(d.prelude[4:8])
	if crc32.ChecksumIEEE(d.prelude[:8]) != binary.BigEndian.Uint32(d.prelude[8:12]) {
		return errors.New("an event-stream message's prelude fails its checksum")
	}
	if total < minMessage || total > maxMessage || headers > total-minMessage {
		return fmt.Errorf("%w: length %d with %d of headers", errMalformed, total, headers)
	}
	rest := int(total) - preludeLen
	if cap(d.buf) < rest {
		d.buf = make([]byte, rest)
	}
	d.buf = d.buf[:rest]
	if _, err := io.ReadFull(src, d.buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	body := d.buf[:rest-4]
	sum := crc32.Update(crc32.ChecksumIEEE(d.prelude[:]), crc32.IEEETable, body)
	if sum != binary.BigEndian.Uint32(d.buf[rest-4:]) {
		return errors.New("an event-stream message fails its checksum")
	}
	*m = message{payload: body[headers:]}
	return m.readHeaders(body[:headers])
}

// readHeaders reads h, a message's headers, keeping the string values of
// those the reframer reads (the last, if a name repeats).
func (m *message) readHeaders(h []byte) error {
	for len(h) > 0 {
		n := int(h[0])
		if len(h) < 1+n+1 {
			return errMalformed
		}
		name, typ := h[1:1+n], h[1+n]
		h = h[2+n:]
		var size int
		switch typ {
		case 0, 1: // true, false
		case 2: // byte
			size = 1
		case 3: // short
			size = 2
		case 4: // integer
			size = 4
		case 5, 8: // long, timestamp
			size = 8
		case 9: // uuid
			size = 16
		case 6, 7: // bytes, string: a 2-byte length first
			if len(h) < 2 {
				return errMalformed
			}
			size = int(binary.BigEndian.Uint16(h))
			h = h[2:]
		default:
			return fmt.Errorf("%w: a header of type %d", errMalformed, typ)
		}
		if len(h) < size {
			return errMalformed
		}
		if v := h[:size]; typ == 7 {
			switch string(name) {
			case ":message-type":
				m.messageType = v
			case ":event-type":
				m.eventType = v
			case ":exception-type":
				m.exceptionType = v
			case ":error-code":
				m.errorCode = v
			}
		}
		h = h[size:]
	}
	return nil
}
