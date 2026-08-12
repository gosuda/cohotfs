package bridgeproto

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestFrameRoundTripAndBounds(t *testing.T) {
	var encoded bytes.Buffer
	want := Frame{StreamID: 17, Kind: Data, Payload: []byte("payload")}
	if err := Write(&encoded, want); err != nil {
		t.Fatal(err)
	}
	got, err := Read(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got.StreamID != want.StreamID || got.Kind != want.Kind || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("frame = %#v, want %#v", got, want)
	}

	var oversized [9]byte
	binary.BigEndian.PutUint32(oversized[0:4], 1)
	oversized[4] = Data
	binary.BigEndian.PutUint32(oversized[5:9], maxPayload+1)
	if _, err := Read(bytes.NewReader(oversized[:])); err == nil {
		t.Fatal("accepted oversized frame")
	}
	if err := Write(&bytes.Buffer{}, Frame{StreamID: 1, Kind: Open, Payload: []byte("unexpected")}); err == nil {
		t.Fatal("accepted control frame payload")
	}
}
