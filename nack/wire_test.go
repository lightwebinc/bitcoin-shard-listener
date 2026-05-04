package nack_test

import (
	"testing"

	"github.com/lightwebinc/bitcoin-shard-listener/nack"
)

func TestEncodeDecodeNACK(t *testing.T) {
	var txid [32]byte
	txid[0] = 0xAB

	n := &nack.NACK{
		MsgType:    nack.MsgTypeNACK,
		TxID:       txid,
		SenderID:   0xAABBCCDD,
		SequenceID: 0x11223344,
		SeqNum:     0xDEADBEEF,
	}

	var buf [nack.NACKSize]byte
	nack.Encode(n, buf[:])

	got, err := nack.Decode(buf[:])
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.MsgType != nack.MsgTypeNACK {
		t.Errorf("MsgType = 0x%02X, want 0x%02X", got.MsgType, nack.MsgTypeNACK)
	}
	if got.TxID != txid {
		t.Errorf("TxID mismatch")
	}
	if got.SenderID != n.SenderID {
		t.Errorf("SenderID = %d, want %d", got.SenderID, n.SenderID)
	}
	if got.SequenceID != n.SequenceID {
		t.Errorf("SequenceID = %d, want %d", got.SequenceID, n.SequenceID)
	}
	if got.SeqNum != n.SeqNum {
		t.Errorf("SeqNum = %d, want %d", got.SeqNum, n.SeqNum)
	}
}

func TestEncodeDecodeACK(t *testing.T) {
	r := &nack.Response{
		MsgType:    nack.MsgTypeACK,
		Flags:      0x03, // multicast_sent | unicast_sent
		SenderID:   0xAABBCCDD,
		SequenceID: 0x11223344,
		SeqNum:     0xDEADBEEF,
	}
	var buf [nack.ResponseSize]byte
	nack.EncodeResponse(r, buf[:])

	got, err := nack.DecodeResponse(buf[:])
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if got.MsgType != nack.MsgTypeACK {
		t.Errorf("MsgType = 0x%02X, want 0x%02X", got.MsgType, nack.MsgTypeACK)
	}
	if got.Flags != 0x03 {
		t.Errorf("Flags = 0x%02X, want 0x03", got.Flags)
	}
	if got.SenderID != r.SenderID {
		t.Errorf("SenderID = 0x%08X, want 0x%08X", got.SenderID, r.SenderID)
	}
	if got.SequenceID != r.SequenceID {
		t.Errorf("SequenceID = 0x%08X, want 0x%08X", got.SequenceID, r.SequenceID)
	}
	if got.SeqNum != r.SeqNum {
		t.Errorf("SeqNum = 0x%08X, want 0x%08X", got.SeqNum, r.SeqNum)
	}
}

func TestDecodeResponse_MISS_24bytes(t *testing.T) {
	r := &nack.Response{
		MsgType:    nack.MsgTypeMISS,
		Flags:      0x00,
		SenderID:   0x12345678,
		SequenceID: 0xABCDEF01,
		SeqNum:     42,
	}
	var buf [nack.ResponseSize]byte
	nack.EncodeResponse(r, buf[:])

	got, err := nack.DecodeResponse(buf[:])
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if got.MsgType != nack.MsgTypeMISS {
		t.Errorf("MsgType = 0x%02X, want 0x%02X", got.MsgType, nack.MsgTypeMISS)
	}
	if got.SenderID != r.SenderID {
		t.Errorf("SenderID mismatch")
	}
	if got.SequenceID != r.SequenceID {
		t.Errorf("SequenceID mismatch")
	}
	if got.SeqNum != r.SeqNum {
		t.Errorf("SeqNum mismatch")
	}
}

func TestDecodeResponse_ACK_24bytes(t *testing.T) {
	r := &nack.Response{
		MsgType:    nack.MsgTypeACK,
		Flags:      0x01,
		SenderID:   999,
		SequenceID: 888,
		SeqNum:     777,
	}
	var buf [nack.ResponseSize]byte
	nack.EncodeResponse(r, buf[:])

	got, err := nack.DecodeResponse(buf[:])
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if got.MsgType != nack.MsgTypeACK {
		t.Errorf("MsgType = 0x%02X, want ACK", got.MsgType)
	}
	if got.Flags != 0x01 {
		t.Errorf("Flags = 0x%02X, want 0x01", got.Flags)
	}
}

func TestDecodeResponse_badMagic(t *testing.T) {
	var buf [nack.ResponseSize]byte
	nack.EncodeResponse(&nack.Response{MsgType: nack.MsgTypeACK}, buf[:])
	buf[0] = 0xFF
	_, err := nack.DecodeResponse(buf[:])
	if err != nack.ErrBadResponse {
		t.Errorf("want ErrBadResponse for bad magic, got %v", err)
	}
}

func TestDecodeResponse_tooShort(t *testing.T) {
	_, err := nack.DecodeResponse(make([]byte, nack.ResponseSize-1))
	if err != nack.ErrBadResponse {
		t.Errorf("want ErrBadResponse for short buf, got %v", err)
	}
}

func TestACKFlags_multicast_unicast(t *testing.T) {
	// Test that multicast_sent (0x01) and unicast_sent (0x02) are independent
	for _, tc := range []struct {
		flags byte
		mc    bool
		uc    bool
	}{
		{0x00, false, false},
		{0x01, true, false},
		{0x02, false, true},
		{0x03, true, true},
	} {
		r := &nack.Response{MsgType: nack.MsgTypeACK, Flags: tc.flags}
		var buf [nack.ResponseSize]byte
		nack.EncodeResponse(r, buf[:])
		got, err := nack.DecodeResponse(buf[:])
		if err != nil {
			t.Fatalf("flags=0x%02X: %v", tc.flags, err)
		}
		mc := got.Flags&0x01 != 0
		uc := got.Flags&0x02 != 0
		if mc != tc.mc || uc != tc.uc {
			t.Errorf("flags=0x%02X: mc=%v uc=%v, want mc=%v uc=%v",
				tc.flags, mc, uc, tc.mc, tc.uc)
		}
	}
}

func TestResponseSize_is24(t *testing.T) {
	if nack.ResponseSize != 24 {
		t.Errorf("ResponseSize = %d, want 24", nack.ResponseSize)
	}
}

func TestDecodeErrShort(t *testing.T) {
	_, err := nack.Decode(make([]byte, nack.NACKSize-1))
	if err != nack.ErrBadNACK {
		t.Errorf("want ErrBadNACK for short buf, got %v", err)
	}
}

func TestDecodeErrBadMagic(t *testing.T) {
	buf := make([]byte, nack.NACKSize)
	buf[0] = 0xFF
	_, err := nack.Decode(buf)
	if err != nack.ErrBadNACK {
		t.Errorf("want ErrBadNACK for bad magic, got %v", err)
	}
}

func TestDecodeErrBadType(t *testing.T) {
	var buf [nack.NACKSize]byte
	n := &nack.NACK{MsgType: nack.MsgTypeNACK}
	nack.Encode(n, buf[:])
	buf[6] = 0x99
	_, err := nack.Decode(buf[:])
	if err != nack.ErrBadNACK {
		t.Errorf("want ErrBadNACK for unknown MsgType, got %v", err)
	}
}

func TestCRC32cSenderID(t *testing.T) {
	// SenderID is now a CRC32c checksum of the IPv6 address (uint32)
	var senderID uint32 = 0xDEADBEEF

	n := &nack.NACK{
		MsgType:    nack.MsgTypeNACK,
		SenderID:   senderID,
		SequenceID: 0x99887766,
	}
	var buf [nack.NACKSize]byte
	nack.Encode(n, buf[:])
	got, err := nack.Decode(buf[:])
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.SenderID != senderID {
		t.Errorf("SenderID not preserved: got %x, want %x", got.SenderID, senderID)
	}
}
