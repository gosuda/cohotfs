// Package bridgeproto defines the bounded binary stream framing used by the
// WSL-to-Windows Chrome companion.
package bridgeproto

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	Open       byte = 1
	Data       byte = 2
	Close      byte = 3
	maxPayload      = 1 << 20
)

type Frame struct {
	StreamID uint32
	Kind     byte
	Payload  []byte
}

func Read(reader io.Reader) (Frame, error) {
	var header [9]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return Frame{}, err
	}
	streamID := binary.BigEndian.Uint32(header[0:4])
	kind := header[4]
	length := binary.BigEndian.Uint32(header[5:9])
	if streamID == 0 || (kind != Open && kind != Data && kind != Close) || length > maxPayload || kind != Data && length != 0 {
		return Frame{}, fmt.Errorf("invalid Windows bridge frame")
	}
	frame := Frame{StreamID: streamID, Kind: kind}
	if length != 0 {
		frame.Payload = make([]byte, length)
		if _, err := io.ReadFull(reader, frame.Payload); err != nil {
			return Frame{}, err
		}
	}
	return frame, nil
}

func Write(writer io.Writer, frame Frame) error {
	if frame.StreamID == 0 || (frame.Kind != Open && frame.Kind != Data && frame.Kind != Close) || len(frame.Payload) > maxPayload || frame.Kind != Data && len(frame.Payload) != 0 {
		return fmt.Errorf("invalid Windows bridge frame")
	}
	var header [9]byte
	binary.BigEndian.PutUint32(header[0:4], frame.StreamID)
	header[4] = frame.Kind
	binary.BigEndian.PutUint32(header[5:9], uint32(len(frame.Payload)))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	if len(frame.Payload) != 0 {
		_, err := writer.Write(frame.Payload)
		return err
	}
	return nil
}
