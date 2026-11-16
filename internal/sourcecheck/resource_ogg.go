package sourcecheck

import (
	"bytes"
	"encoding/binary"
)

// This validates Ogg pages and the codec identification packet, not compressed
// audio frames. Duration is only derived from a complete, single-stream EOS.
func identifyOgg(data []byte, out *ResourceResult) {
	out.Format = "ogg"
	out.Signature = "ogg_pages"
	out.ReasonCode = "ogg_evidence_incomplete"
	out.Reason = "已发现 Ogg 容器，读取范围内尚无完整音轨证据"
	var serial, sequence uint32
	var rate, preSkip uint64
	var finalGranule uint64
	var firstPacket []byte
	packetOpen, ended := false, false
	packets, pages := 0, 0
	codec := ""
	headerPackets := 0
	offset := 0
	for offset < len(data) {
		pages++
		if pages > 4096 {
			out.ReasonCode = "ogg_page_limit"
			out.Reason = "Ogg 页数量超出本次结构检查预算"
			return
		}
		if len(data)-offset < 27 {
			break
		}
		header := data[offset : offset+27]
		if string(header[:4]) != "OggS" || header[4] != 0 || header[5]&^byte(7) != 0 {
			out.ReasonCode = "ogg_invalid_structure"
			out.Reason = "Ogg 页头结构未通过校验"
			return
		}
		segments := int(header[26])
		if len(data)-offset < 27+segments {
			break
		}
		laces := data[offset+27 : offset+27+segments]
		payloadSize := 0
		for _, size := range laces {
			payloadSize += int(size)
		}
		pageSize := 27 + segments + payloadSize
		if len(data)-offset < pageSize {
			break
		}
		page := data[offset : offset+pageSize]
		if oggChecksum(page) != binary.LittleEndian.Uint32(header[22:26]) {
			out.ReasonCode = "ogg_checksum_mismatch"
			out.Reason = "Ogg 页校验和不匹配，不能确认有效音轨结构"
			return
		}
		pageSerial := binary.LittleEndian.Uint32(header[14:18])
		pageSequence := binary.LittleEndian.Uint32(header[18:22])
		if pages == 1 {
			if header[5]&2 == 0 || header[5]&1 != 0 || pageSequence != 0 {
				out.ReasonCode = "ogg_invalid_structure"
				out.Reason = "Ogg 起始页或序列号不符合单音轨结构"
				return
			}
			serial = pageSerial
		} else if pageSerial != serial || ended || header[5]&2 != 0 {
			out.ReasonCode = "ogg_multiple_streams_unsupported"
			out.Reason = "Ogg 包含串联或多个逻辑流，本次检查不推断整体音频时长"
			return
		} else if pageSequence != sequence+1 || (header[5]&1 != 0) != packetOpen {
			out.ReasonCode = "ogg_invalid_structure"
			out.Reason = "Ogg 页序列或跨页数据包不连续"
			return
		}
		sequence = pageSequence
		position := 27 + segments
		for _, size := range laces {
			if packets == 0 {
				if len(firstPacket)+int(size) > 64<<10 {
					out.ReasonCode = "ogg_identification_limit"
					out.Reason = "Ogg 编码标识包超出检查预算"
					return
				}
				firstPacket = append(firstPacket, page[position:position+int(size)]...)
			}
			position += int(size)
			packetOpen = size == 255
			if packetOpen {
				continue
			}
			packets++
			if packets != 1 {
				continue
			}
			switch {
			case len(firstPacket) == 30 && bytes.Equal(firstPacket[:7], []byte{1, 'v', 'o', 'r', 'b', 'i', 's'}):
				block := firstPacket[28]
				if binary.LittleEndian.Uint32(firstPacket[7:11]) != 0 || firstPacket[11] == 0 || firstPacket[29] != 1 || block&15 < 6 || block>>4 > 13 || block&15 > block>>4 {
					break
				}
				rate = uint64(binary.LittleEndian.Uint32(firstPacket[12:16]))
				codec, headerPackets = "vorbis", 3
			case len(firstPacket) >= 19 && string(firstPacket[:8]) == "OpusHead":
				channels, family := firstPacket[9], firstPacket[18]
				if firstPacket[8] == 0 || firstPacket[8] > 15 || channels == 0 {
					break
				}
				if family == 0 {
					if len(firstPacket) != 19 || channels > 2 {
						break
					}
				} else {
					// Keep the supported subset precise; mapping families need
					// their own validation before accepting multichannel audio.
					break
				}
				rate, preSkip = 48000, uint64(binary.LittleEndian.Uint16(firstPacket[10:12]))
				codec, headerPackets = "opus", 2
			}
			if codec == "" || rate == 0 || rate > 768000 {
				out.ReasonCode = "ogg_codec_unsupported"
				out.Reason = "已识别 Ogg 容器，编码标识未匹配本次支持的 Vorbis/Opus 结构"
				return
			}
			out.Codec = codec
		}
		ended = header[5]&4 != 0
		if ended {
			if packetOpen {
				out.ReasonCode = "ogg_invalid_structure"
				out.Reason = "Ogg 结束页仍含未完成的数据包"
				return
			}
			finalGranule = binary.LittleEndian.Uint64(header[6:14])
		}
		offset += pageSize
	}
	if codec == "" || packets <= headerPackets {
		return
	}
	if out.Complete && (offset != len(data) || !ended) {
		return
	}
	out.Status = "recognized_audio"
	out.Signature = "ogg_" + codec + "_pages"
	out.ReasonCode, out.Reason = "", ""
	if out.Complete && ended && finalGranule > preSkip && finalGranule-preSkip <= rate*86400 {
		ms := int64((finalGranule - preSkip) * 1000 / rate)
		if ms > 0 {
			out.DurationMS = &ms
			out.DurationMethod = "ogg_eos_granule"
		}
	}
}

var oggCRCTable = func() (table [256]uint32) {
	for i := range table {
		value := uint32(i) << 24
		for bit := 0; bit < 8; bit++ {
			if value&0x80000000 != 0 {
				value = value<<1 ^ 0x04c11db7
			} else {
				value <<= 1
			}
		}
		table[i] = value
	}
	return
}()

func oggChecksum(page []byte) uint32 {
	var checksum uint32
	for i, value := range page {
		if i >= 22 && i < 26 {
			value = 0
		}
		checksum = checksum<<8 ^ oggCRCTable[byte(checksum>>24)^value]
	}
	return checksum
}
