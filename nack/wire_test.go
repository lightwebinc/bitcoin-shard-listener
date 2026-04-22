package nack_test

import (
	"testing"

	"github.com/lightwebinc/bitcoin-shard-listener/nack"
)

func TestEncodeDecodeNACK(t *testing.T) {
	var txid [32]byte
	txid[0] = 0xAB

	var senderID [16]byte
	senderID[15] = 0x01

	n := &nack.NACK{
		MsgType:     nack.MsgTypeNACK,
		TxID:        txid,
		ShardSeqNum: 0xDEADBEEF00000001,
		SenderID:    senderID,
		SequenceID:  0x1122334455667788,
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
	if got.ShardSeqNum != n.ShardSeqNum {
		t.Errorf("ShardSeqNum = %d, want %d", got.ShardSeqNum, n.ShardSeqNum)
	}
	if got.SenderID != senderID {
		t.Errorf("SenderID mismatch: got %x, want %x", got.SenderID, senderID)
	}
	if got.SequenceID != n.SequenceID {
		t.Errorf("SequenceID = %d, want %d", got.SequenceID, n.SequenceID)
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

func TestIPv4MappedSenderID(t *testing.T) {
	var senderID [16]byte
	// ::ffff:192.0.2.1
	senderID[10] = 0xFF
	senderID[11] = 0xFF
	senderID[12] = 192
	senderID[13] = 0
	senderID[14] = 2
	senderID[15] = 1

	n := &nack.NACK{
		MsgType:    nack.MsgTypeNACK,
		SenderID:   senderID,
		SequenceID: 0x9988776655443322,
	}
	var buf [nack.NACKSize]byte
	nack.Encode(n, buf[:])
	got, err := nack.Decode(buf[:])
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.SenderID != senderID {
		t.Errorf("IPv4-mapped SenderID not preserved: got %x, want %x", got.SenderID, senderID)
	}
}
