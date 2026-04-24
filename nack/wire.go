// Package nack implements NORM-inspired multicast gap recovery for
// bitcoin-shard-listener. This file defines the 56-byte NACK wire format (BRC-123).
//
// # NACK datagram (UDP, 56 bytes, 8-byte aligned)
//
//	Offset  Size  Field
//	------  ----  -----
//	     0     4  Magic (0xE3E1F3E8)  — BSV mainnet magic for tap/mirror/span identification
//	     4     2  ProtoVer (0x02BF)
//	     6     1  MsgType = 0x10 (NACK) or 0x11 (MISS)
//	     7     1  Reserved = 0x00
//	     8    32  TxID               — identifies the missing frame
//	    40     4  SenderID           — CRC32c of IPv6; 0 = unknown
//	    44     4  SequenceID         — flow identifier; 0 = unknown
//	    48     4  ShardSeqNum        — sender's sequence number; 0 = unknown
//	    52     4  Reserved           — padding; must be 0x00000000
//
// The MISS datagram is the optional 8-byte "not found" response from a retry
// endpoint (MsgType=0x11). Only the first 8 bytes are meaningful.
package nack

import (
	"encoding/binary"
	"errors"
)

const (
	// NACKSize is the fixed size of a NACK datagram in bytes.
	NACKSize = 56

	// MsgTypeNACK identifies a gap-retransmission request.
	MsgTypeNACK byte = 0x10

	// MsgTypeMISS identifies a "frame not in cache" response from a retry endpoint.
	MsgTypeMISS byte = 0x11

	nackMagic    uint32 = 0xE3E1F3E8
	nackProtoVer uint16 = 0x02BF
)

// ErrBadNACK is returned when a received datagram does not decode as a valid NACK.
var ErrBadNACK = errors.New("nack: invalid datagram")

// NACK is the in-memory representation of a NACK or MISS datagram.
type NACK struct {
	MsgType     byte
	TxID        [32]byte
	SenderID    uint32
	SequenceID  uint32
	ShardSeqNum uint32
}

// Encode serialises n into buf (must be at least [NACKSize] bytes).
func Encode(n *NACK, buf []byte) {
	_ = buf[NACKSize-1] // bounds check
	binary.BigEndian.PutUint32(buf[0:4], nackMagic)
	binary.BigEndian.PutUint16(buf[4:6], nackProtoVer)
	buf[6] = n.MsgType
	buf[7] = 0
	copy(buf[8:40], n.TxID[:])
	binary.BigEndian.PutUint32(buf[40:44], n.SenderID)
	binary.BigEndian.PutUint32(buf[44:48], n.SequenceID)
	binary.BigEndian.PutUint32(buf[48:52], n.ShardSeqNum)
	binary.BigEndian.PutUint32(buf[52:56], 0) // reserved padding
}

// Decode parses a NACK or MISS datagram from buf.
// Returns [ErrBadNACK] if the datagram is too short, magic is wrong, or
// MsgType is unrecognised.
func Decode(buf []byte) (*NACK, error) {
	if len(buf) < NACKSize {
		return nil, ErrBadNACK
	}
	if binary.BigEndian.Uint32(buf[0:4]) != nackMagic {
		return nil, ErrBadNACK
	}
	mt := buf[6]
	if mt != MsgTypeNACK && mt != MsgTypeMISS {
		return nil, ErrBadNACK
	}
	n := &NACK{MsgType: mt}
	copy(n.TxID[:], buf[8:40])
	n.SenderID = binary.BigEndian.Uint32(buf[40:44])
	n.SequenceID = binary.BigEndian.Uint32(buf[44:48])
	n.ShardSeqNum = binary.BigEndian.Uint32(buf[48:52])
	return n, nil
}

// DecodeMISS returns true if buf is a valid MISS response (8-byte minimum).
// Only the first 8 bytes are examined.
func DecodeMISS(buf []byte) bool {
	if len(buf) < 8 {
		return false
	}
	return binary.BigEndian.Uint32(buf[0:4]) == nackMagic &&
		buf[6] == MsgTypeMISS
}
