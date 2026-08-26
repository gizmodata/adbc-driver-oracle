package oratype

import "encoding/binary"

const base64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

func convertBase64(buf []byte, value uint64, size, offset int) int {
	for i := 0; i < size; i++ {
		buf[offset+size-i-1] = base64Alphabet[value&0x3f]
		value >>= 6
	}
	return offset + size
}

// EncodeRowid formats a physical rowid (rba, partition id, block, slot) as
// the 18-character string Oracle displays. Returns "" for an all-zero rowid.
func EncodeRowid(rba uint32, partitionID uint16, blockNum uint32, slotNum uint16) string {
	if rba == 0 && partitionID == 0 && blockNum == 0 && slotNum == 0 {
		return ""
	}
	var buf [18]byte
	off := convertBase64(buf[:], uint64(rba), 6, 0)
	off = convertBase64(buf[:], uint64(partitionID), 3, off)
	off = convertBase64(buf[:], uint64(blockNum), 6, off)
	convertBase64(buf[:], uint64(slotNum), 3, off)
	return string(buf[:])
}

// DecodeURowid converts the raw bytes of a universal rowid into its string
// form (physical rowids are rendered like EncodeRowid; logical ones are
// base64-ish encoded with a leading '*').
func DecodeURowid(in []byte) string {
	if len(in) < 13 {
		return ""
	}
	if in[0] == 1 {
		return EncodeRowid(binary.BigEndian.Uint32(in[1:]), binary.BigEndian.Uint16(in[5:]),
			binary.BigEndian.Uint32(in[7:]), binary.BigEndian.Uint16(in[11:]))
	}
	inputLen := len(in)
	outputLen := (inputLen / 3) * 4
	switch inputLen % 3 {
	case 1:
		outputLen++
	case 2:
		outputLen += 3
	}
	out := make([]byte, 0, outputLen)
	out = append(out, '*')
	inputLen--
	off := 1
	for inputLen > 0 {
		pos := in[off] >> 2
		out = append(out, base64Alphabet[pos])
		pos = (in[off] & 0x3) << 4
		if inputLen == 1 {
			out = append(out, base64Alphabet[pos])
			break
		}
		off++
		pos |= (in[off] & 0xf0) >> 4
		out = append(out, base64Alphabet[pos])
		pos = (in[off] & 0xf) << 2
		if inputLen == 2 {
			out = append(out, base64Alphabet[pos])
			break
		}
		off++
		pos |= (in[off] & 0xc0) >> 6
		out = append(out, base64Alphabet[pos])
		pos = in[off] & 0x3f
		out = append(out, base64Alphabet[pos])
		off++
		inputLen -= 3
	}
	return string(out)
}
