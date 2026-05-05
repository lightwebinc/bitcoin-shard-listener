// Package nack implements NORM-inspired multicast gap recovery for
// bitcoin-shard-listener. This file defines the 24-byte NACK request wire
// format and the 16-byte ACK/MISS response wire format.
//
// # NACK datagram (UDP, 24 bytes, 8-byte aligned)
//
//	Offset  Size  Field
//	------  ----  -----
//	     0     4  Magic (0xE3E1F3E8) — BSV mainnet magic
//	     4     2  ProtoVer (0x02BF)
//	     6     1  MsgType = 0x10 (NACK)
//	     7     1  LookupType: 0x00 = by PrevSeq, 0x01 = by CurSeq
//	     8     8  LookupSeq (uint64 BE) — hash value to look up
//	    16     8  Reserved (must be 0x0000000000000000)
//
// LookupType 0x00 (by PrevSeq): the retry endpoint searches its secondary
// index for a frame whose PrevSeq == LookupSeq, returning the frame that
// immediately follows the last known good frame in the chain.
//
// LookupType 0x01 (by CurSeq): the retry endpoint searches its primary index
// for a frame whose CurSeq == LookupSeq, returning the last frame before the
// gap as seen by the receiver.
//
// Dispatching both lookup types in parallel enables recovery of both ends of
// a multi-frame gap in a single round-trip.
//
// # Response datagram (MISS/ACK, UDP, 16 bytes)
//
//	Offset  Size  Field
//	------  ----  -----
//	     0     4  Magic (0xE3E1F3E8)
//	     4     2  ProtoVer (0x02BF)
//	     6     1  MsgType = 0x11 (MISS) or 0x12 (ACK)
//	     7     1  Flags (ACK: 0x01=multicast_sent, 0x02=unicast_sent)
//	     8     8  CurSeq of the retrieved frame (0 for MISS)
package nack

import (
	"encoding/binary"
	"errors"
)

const (
	// NACKSize is the fixed size of a NACK datagram in bytes.
	NACKSize = 24

	// MsgTypeNACK identifies a gap-retransmission request.
	MsgTypeNACK byte = 0x10

	// MsgTypeMISS identifies a "frame not in cache" response from a retry endpoint.
	MsgTypeMISS byte = 0x11

	// MsgTypeACK identifies a "frame found, retransmit dispatched" response
	// from a retry endpoint.
	MsgTypeACK byte = 0x12

	// LookupByPrevSeq requests the frame whose PrevSeq equals LookupSeq.
	// This is the forward lookup: "give me the frame right after the last known one."
	LookupByPrevSeq byte = 0x00

	// LookupByCurSeq requests the frame whose CurSeq equals LookupSeq.
	// This is the backward lookup: "give me the frame right before the gap."
	LookupByCurSeq byte = 0x01

	// ResponseSize is the fixed size of a MISS or ACK response datagram.
	ResponseSize = 16

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

// NACK is the in-memory representation of a 24-byte NACK datagram.
type NACK struct {
	MsgType    byte   // MsgTypeNACK
	LookupType byte   // LookupByPrevSeq or LookupByCurSeq
	LookupSeq  uint64 // XXH64 value to look up
}

// Encode serialises n into buf (must be at least [NACKSize] bytes).
func Encode(n *NACK, buf []byte) {
	_ = buf[NACKSize-1] // bounds check
	binary.BigEndian.PutUint32(buf[0:4], nackMagic)
	binary.BigEndian.PutUint16(buf[4:6], nackProtoVer)
	buf[6] = n.MsgType
	buf[7] = n.LookupType
	binary.BigEndian.PutUint64(buf[8:16], n.LookupSeq)
	binary.BigEndian.PutUint64(buf[16:24], 0) // reserved
}

// Decode parses a NACK datagram from buf.
// Returns [ErrBadNACK] if the datagram is too short, magic is wrong, or
// MsgType is not MsgTypeNACK.
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
	return &NACK{
		MsgType:    mt,
		LookupType: buf[7],
		LookupSeq:  binary.BigEndian.Uint64(buf[8:16]),
	}, nil
}

// Response is the in-memory representation of a 16-byte MISS or ACK response.
type Response struct {
	MsgType byte   // MsgTypeMISS or MsgTypeACK
	Flags   byte   // ACK flags: 0x01=multicast_sent, 0x02=unicast_sent
	CurSeq  uint64 // CurSeq of the retrieved frame; 0 for MISS
}

// EncodeResponse serialises r into buf (must be at least [ResponseSize] bytes).
func EncodeResponse(r *Response, buf []byte) {
	_ = buf[ResponseSize-1] // bounds check
	binary.BigEndian.PutUint32(buf[0:4], nackMagic)
	binary.BigEndian.PutUint16(buf[4:6], nackProtoVer)
	buf[6] = r.MsgType
	buf[7] = r.Flags
	binary.BigEndian.PutUint64(buf[8:16], r.CurSeq)
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
		MsgType: mt,
		Flags:   buf[7],
		CurSeq:  binary.BigEndian.Uint64(buf[8:16]),
	}, nil
}
