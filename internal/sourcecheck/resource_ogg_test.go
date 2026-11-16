package sourcecheck

import (
	"encoding/binary"
	"testing"
)

// Independent bit-at-a-time CRC construction keeps fixtures independent from
// the production table-driven page checker.
func oggFixturePage(flags byte, sequence uint32, granule uint64, packets ...[]byte) []byte {
	var laces, payload []byte
	for _, packet := range packets {
		for len(packet) >= 255 {
			laces = append(laces, 255)
			payload = append(payload, packet[:255]...)
			packet = packet[255:]
		}
		laces = append(laces, byte(len(packet)))
		payload = append(payload, packet...)
	}
	page := make([]byte, 27)
	copy(page, "OggS")
	page[5] = flags
	binary.LittleEndian.PutUint64(page[6:14], granule)
	binary.LittleEndian.PutUint32(page[14:18], 42)
	binary.LittleEndian.PutUint32(page[18:22], sequence)
	page[26] = byte(len(laces))
	page = append(append(page, laces...), payload...)
	var crc uint32
	for _, value := range page {
		crc ^= uint32(value) << 24
		for bit := 0; bit < 8; bit++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	binary.LittleEndian.PutUint32(page[22:26], crc)
	return page
}

func oggFixture(codec string, seconds uint64) []byte {
	if codec == "opus" {
		head := make([]byte, 19)
		copy(head, "OpusHead")
		head[8] = 1
		head[9] = 2
		binary.LittleEndian.PutUint16(head[10:12], 312)
		return append(oggFixturePage(2, 0, 0, head), oggFixturePage(4, 1, seconds*48000+312, []byte("OpusTags"), []byte{0xf8, 0xff, 0xfe})...)
	}
	head := make([]byte, 30)
	copy(head, []byte{1, 'v', 'o', 'r', 'b', 'i', 's'})
	head[11] = 2
	binary.LittleEndian.PutUint32(head[12:16], 44100)
	head[28] = 0xb8
	head[29] = 1
	return append(oggFixturePage(2, 0, 0, head), oggFixturePage(4, 1, seconds*44100, []byte{3, 'v', 'o', 'r', 'b', 'i', 's'}, []byte{5, 'v', 'o', 'r', 'b', 'i', 's'}, []byte{0, 1, 2})...)
}

func TestOggRecognizesStructureAndOnlyUsesCompleteEOSDuration(t *testing.T) {
	for _, codec := range []string{"vorbis", "opus"} {
		t.Run(codec, func(t *testing.T) {
			data := oggFixture(codec, 212)
			for _, complete := range []bool{false, true} {
				out := ResourceResult{Status: "unverified", Complete: complete}
				identifyAudio(data, &out)
				if out.Status != "recognized_audio" || out.Format != "ogg" || out.Codec != codec {
					t.Fatalf("%+v", out)
				}
				if complete {
					if out.DurationMS == nil || *out.DurationMS != 212000 || out.DurationMethod != "ogg_eos_granule" {
						t.Fatalf("%+v", out)
					}
				} else if out.DurationMS != nil {
					t.Fatal("partial file invented duration")
				}
			}
			for _, bad := range [][]byte{data[:16], data[:len(data)-1], append(append([]byte{}, data...), data...)} {
				out := ResourceResult{Status: "unverified", Complete: true}
				identifyAudio(bad, &out)
				if out.Status == "recognized_audio" || out.DurationMS != nil {
					t.Fatalf("invalid/chained/truncated Ogg accepted: %+v", out)
				}
			}
			broken := append([]byte{}, data...)
			broken[len(broken)-1] ^= 1
			out := ResourceResult{Status: "unverified", Complete: true}
			identifyAudio(broken, &out)
			if out.ReasonCode != "ogg_checksum_mismatch" || out.Status == "recognized_audio" {
				t.Fatalf("%+v", out)
			}
		})
	}
}

func TestOggRejectsUnsupportedAndInvalidIdentification(t *testing.T) {
	for _, packet := range [][]byte{[]byte("not an audio codec"), append([]byte("OpusHead"), make([]byte, 11)...)} {
		out := ResourceResult{Status: "unverified", Complete: true}
		identifyAudio(oggFixturePage(2|4, 0, 0, packet), &out)
		if out.ReasonCode != "ogg_codec_unsupported" || out.Status == "recognized_audio" {
			t.Fatalf("%+v", out)
		}
	}
	// A valid container and codec header without audio packets is not enough.
	data := oggFixture("opus", 60)
	firstSize := 27 + int(data[26]) + 19
	out := ResourceResult{Status: "unverified", Complete: true}
	identifyAudio(data[:firstSize], &out)
	if out.Status == "recognized_audio" || out.DurationMS != nil {
		t.Fatalf("%+v", out)
	}
}
