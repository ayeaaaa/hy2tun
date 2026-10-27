package control

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// MaxFrameSize is the maximum payload size of a single frame, excluding
	// the 4-byte length prefix. Larger frames are rejected before any body
	// allocation so a hostile peer cannot exhaust memory.
	MaxFrameSize = 1 << 20

	// ProtocolVersion is the control protocol version carried in Hello.
	ProtocolVersion = 1

	frameHeaderSize = 4
)

// WriteMessage writes msg as one length-prefixed JSON frame: a 4-byte
// big-endian payload length followed by the JSON payload.
func WriteMessage(w io.Writer, msg any) error {
	if msg == nil {
		return fmt.Errorf("%w: nil message", ErrBadMessage)
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("%w: marshal: %w", ErrBadMessage, err)
	}
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("%w: %d bytes exceeds limit of %d", ErrFrameTooLarge, len(payload), MaxFrameSize)
	}

	var header [frameHeaderSize]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeFull(w, header[:]); err != nil {
		return fmt.Errorf("control: write frame header: %w", err)
	}
	if err := writeFull(w, payload); err != nil {
		return fmt.Errorf("control: write frame body: %w", err)
	}
	return nil
}

// ReadMessage reads one length-prefixed JSON frame and decodes it into the
// message type named by its "type" field. It returns ErrFrameTooLarge for an
// oversized length, ErrBadMessage for an empty or malformed payload, and
// ErrUnknownType for an unrecognized message type.
func ReadMessage(r io.Reader) (any, error) {
	var header [frameHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, fmt.Errorf("control: read frame header: %w", err)
	}

	size := binary.BigEndian.Uint32(header[:])
	if size == 0 {
		return nil, fmt.Errorf("%w: empty frame", ErrBadMessage)
	}
	if size > MaxFrameSize {
		return nil, fmt.Errorf("%w: %d bytes exceeds limit of %d", ErrFrameTooLarge, size, MaxFrameSize)
	}

	// size is bounded by MaxFrameSize, so this allocation is bounded.
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("control: read frame body (%d bytes): %w", size, err)
	}

	return decodeMessage(payload)
}

// decodeMessage decodes a frame payload into its concrete message type.
func decodeMessage(payload []byte) (any, error) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadMessage, err)
	}
	msg, ok := newMessage(envelope.Type)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownType, envelope.Type)
	}
	if err := json.Unmarshal(payload, msg); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadMessage, err)
	}
	return msg, nil
}

// writeFull writes all of p, retrying short writes and reporting a stalled
// writer as io.ErrShortWrite.
func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(p) {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
