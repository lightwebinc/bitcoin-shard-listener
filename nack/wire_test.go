package nack_test

import (
	"testing"

	"github.com/lightwebinc/bitcoin-shard-listener/nack"
)

func TestNACKSize(t *testing.T) {
	if nack.NACKSize != 24 {
		t.Errorf("NACKSize = %d, want 24", nack.NACKSize)
	}
}

func TestResponseSize(t *testing.T) {
	if nack.ResponseSize != 16 {
		t.Errorf("ResponseSize = %d, want 16", nack.ResponseSize)
	}
}

func TestEncodeDecodeNACK_ByPrevSeq(t *testing.T) {
	n := &nack.NACK{
		MsgType:    nack.MsgTypeNACK,
		LookupType: nack.LookupByPrevSeq,
		LookupSeq:  0xDEADBEEFCAFEBABE,
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
	if got.LookupType != nack.LookupByPrevSeq {
		t.Errorf("LookupType = 0x%02X, want LookupByPrevSeq", got.LookupType)
	}
	if got.LookupSeq != n.LookupSeq {
		t.Errorf("LookupSeq = 0x%016X, want 0x%016X", got.LookupSeq, n.LookupSeq)
	}
}

func TestEncodeDecodeNACK_ByCurSeq(t *testing.T) {
	n := &nack.NACK{
		MsgType:    nack.MsgTypeNACK,
		LookupType: nack.LookupByCurSeq,
		LookupSeq:  0x0102030405060708,
	}
	var buf [nack.NACKSize]byte
	nack.Encode(n, buf[:])

	got, err := nack.Decode(buf[:])
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.LookupType != nack.LookupByCurSeq {
		t.Errorf("LookupType = 0x%02X, want LookupByCurSeq", got.LookupType)
	}
	if got.LookupSeq != n.LookupSeq {
		t.Errorf("LookupSeq mismatch")
	}
}

func TestDecodeNACK_ErrShort(t *testing.T) {
	_, err := nack.Decode(make([]byte, nack.NACKSize-1))
	if err != nack.ErrBadNACK {
		t.Errorf("want ErrBadNACK for short buf, got %v", err)
	}
}

func TestDecodeNACK_ErrBadMagic(t *testing.T) {
	buf := make([]byte, nack.NACKSize)
	buf[0] = 0xFF
	_, err := nack.Decode(buf)
	if err != nack.ErrBadNACK {
		t.Errorf("want ErrBadNACK for bad magic, got %v", err)
	}
}

func TestDecodeNACK_ErrBadMsgType(t *testing.T) {
	var buf [nack.NACKSize]byte
	nack.Encode(&nack.NACK{MsgType: nack.MsgTypeNACK}, buf[:])
	buf[6] = 0x99
	_, err := nack.Decode(buf[:])
	if err != nack.ErrBadNACK {
		t.Errorf("want ErrBadNACK for unknown MsgType, got %v", err)
	}
}

func TestEncodeDecodeACK(t *testing.T) {
	r := &nack.Response{
		MsgType: nack.MsgTypeACK,
		Flags:   0x03, // multicast_sent | unicast_sent
		CurSeq:  0xAABBCCDDEEFF0011,
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
	if got.Flags != 0x03 {
		t.Errorf("Flags = 0x%02X, want 0x03", got.Flags)
	}
	if got.CurSeq != r.CurSeq {
		t.Errorf("CurSeq = 0x%016X, want 0x%016X", got.CurSeq, r.CurSeq)
	}
}

func TestEncodeDecodeMISS(t *testing.T) {
	r := &nack.Response{
		MsgType: nack.MsgTypeMISS,
		Flags:   0x00,
		CurSeq:  0, // zero for MISS
	}
	var buf [nack.ResponseSize]byte
	nack.EncodeResponse(r, buf[:])

	got, err := nack.DecodeResponse(buf[:])
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if got.MsgType != nack.MsgTypeMISS {
		t.Errorf("MsgType = 0x%02X, want MISS", got.MsgType)
	}
	if got.CurSeq != 0 {
		t.Errorf("CurSeq = %d, want 0 for MISS", got.CurSeq)
	}
}

func TestDecodeResponse_ErrShort(t *testing.T) {
	_, err := nack.DecodeResponse(make([]byte, nack.ResponseSize-1))
	if err != nack.ErrBadResponse {
		t.Errorf("want ErrBadResponse for short buf, got %v", err)
	}
}

func TestDecodeResponse_ErrBadMagic(t *testing.T) {
	var buf [nack.ResponseSize]byte
	nack.EncodeResponse(&nack.Response{MsgType: nack.MsgTypeACK}, buf[:])
	buf[0] = 0xFF
	_, err := nack.DecodeResponse(buf[:])
	if err != nack.ErrBadResponse {
		t.Errorf("want ErrBadResponse for bad magic, got %v", err)
	}
}

func TestACKFlags_multicast_unicast(t *testing.T) {
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
