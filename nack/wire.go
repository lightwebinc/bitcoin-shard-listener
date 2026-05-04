// Package nack implements NORM-inspired multicast gap recovery for
// bitcoin-shard-listener. This file defines the 56-byte NACK request wire format
// and the 24-byte ACK/MISS response wire format (BRC-125).
//
// # NACK datagram (UDP, 56 bytes, 8-byte aligned)
//
//	Offset  Size  Field
//	------  ----  -----
//	     0     4  Magic (0xE3E1F3E8)  — BSV mainnet magic for tap/mirror/span identification
//	     4     2  ProtoVer (0x02BF)
//	     6     1  MsgType = 0x10 (NACK)
//	     7     1  Reserved = 0x00
//	     8    32  TxID               — identifies the missing frame
//	    40     4  SenderID           — CRC32c of IPv6; 0 = unknown
//	    44     4  SequenceID         — flow identifier; 0 = unknown
//	    48     4  SeqNum             — sender's sequence number; 0 = unknown
//	    52     4  Reserved           — padding; must be 0x00000000
//
// # Response datagram (MISS/ACK, UDP, 24 bytes)
//
//	Offset  Size  Field
//	------  ----  -----
//	     0     4  Magic (0xE3E1F3E8)
//	     4     2  ProtoVer (0x02BF)
//	     6     1  MsgType = 0x11 (MISS) or 0x12 (ACK)
//	     7     1  Flags (ACK: 0x01=multicast_sent, 0x02=unicast_sent)
//	     8     4  SenderID    — echoed from NACK
//	    12     4  SequenceID  — echoed from NACK
//	    16     4  SeqNum      — echoed from NACK
//	    20     4  Reserved
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

	// MsgTypeACK identifies a "frame found, retransmit dispatched" response
	// from a retry endpoint (BRC-125).
	MsgTypeACK byte = 0x12

	// ResponseSize is the fixed size of a MISS or ACK response datagram.
	ResponseSize = 24

	nackMagic    uint32 = 0xE3E1F3E8
	nackProtoVer uint16 = 0x02BF
)

// Sentinel errors.
var (
	// ErrBadNACK is returned when a received datagram does not decode as a valid NACK.
	ErrBadNACK = errors.New("nack: invalid datagram")

	// ErrBadResponse is returned when a received datagram does not decode as a
	// valid MISS or ACK response.
	ErrBadResponse = errors.New("nack: invalid response datagram")
)

// NACK is the in-memory representation of a NACK or MISS datagram.
type NACK struct {
	MsgType    byte
	TxID       [32]byte
	SenderID   uint32
	SequenceID uint32
	SeqNum     uint32
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
	binary.BigEndian.PutUint32(buf[48:52], n.SeqNum)
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
	if mt != MsgTypeNACK {
		return nil, ErrBadNACK
	}
	n := &NACK{MsgType: mt}
	copy(n.TxID[:], buf[8:40])
	n.SenderID = binary.BigEndian.Uint32(buf[40:44])
	n.SequenceID = binary.BigEndian.Uint32(buf[44:48])
	n.SeqNum = binary.BigEndian.Uint32(buf[48:52])
	return n, nil
}

// Response is the in-memory representation of a 24-byte MISS or ACK response.
type Response struct {
	MsgType    byte
	Flags      byte
	SenderID   uint32
	SequenceID uint32
	SeqNum     uint32
}

// EncodeResponse serialises r into buf (must be at least [ResponseSize] bytes).
func EncodeResponse(r *Response, buf []byte) {
	_ = buf[ResponseSize-1] // bounds check
	binary.BigEndian.PutUint32(buf[0:4], nackMagic)
	binary.BigEndian.PutUint16(buf[4:6], nackProtoVer)
	buf[6] = r.MsgType
	buf[7] = r.Flags
	binary.BigEndian.PutUint32(buf[8:12], r.SenderID)
	binary.BigEndian.PutUint32(buf[12:16], r.SequenceID)
	binary.BigEndian.PutUint32(buf[16:20], r.SeqNum)
	binary.BigEndian.PutUint32(buf[20:24], 0) // reserved
}

// DecodeResponse parses a MISS or ACK response from buf.
// Returns [ErrBadResponse] if the datagram is too short, magic is wrong,
// or MsgType is not MISS or ACK.
func DecodeResponse(buf []byte) (*Response, error) {
	if len(buf) < ResponseSize {
		return nil, ErrBadResponse
	}
	if binary.BigEndian.Uint32(buf[0:4]) != nackMagic {
		return nil, ErrBadResponse
	}
	mt := buf[6]
	if mt != MsgTypeMISS && mt != MsgTypeACK {
		return nil, ErrBadResponse
	}
	return &Response{
		MsgType:    mt,
		Flags:      buf[7],
		SenderID:   binary.BigEndian.Uint32(buf[8:12]),
		SequenceID: binary.BigEndian.Uint32(buf[12:16]),
		SeqNum:     binary.BigEndian.Uint32(buf[16:20]),
	}, nil
}
