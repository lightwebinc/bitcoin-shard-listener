package nack_test

import (
	"testing"

	"github.com/lightwebinc/bitcoin-shard-listener/nack"
)

func TestEncodeDecodeNACK(t *testing.T) {
	var txid [32]byte
	txid[0] = 0xAB

	n := &nack.NACK{
		MsgType:     nack.MsgTypeNACK,
		TxID:        txid,
		SenderID:    0xAABBCCDD,
		SequenceID:  0x11223344,
		ShardSeqNum: 0xDEADBEEF,
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
	if got.ShardSeqNum != n.ShardSeqNum {
		t.Errorf("ShardSeqNum = %d, want %d", got.ShardSeqNum, n.ShardSeqNum)
	}
}

func TestEncodeMISS(t *testing.T) {
	n := &nack.NACK{MsgType: nack.MsgTypeMISS}
	var buf [nack.NACKSize]byte
	nack.Encode(n, buf[:])
	if !nack.DecodeMISS(buf[:]) {
		t.Error("DecodeMISS should return true for a MISS datagram")
	}
	if nack.DecodeMISS(buf[1:]) {
		t.Error("DecodeMISS should return false for a truncated datagram")
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
