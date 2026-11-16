package sourcecheck

import (
	"bytes"
	"errors"
	"strings"

	mp4 "github.com/abema/go-mp4"
)

type mp4AudioTrack struct {
	sound     bool
	entry     bool
	codec     string
	timescale uint32
	duration  uint64
}

// Only traverse metadata needed for audio evidence. Never expand sample tables,
// artwork, or media payload; the supplied buffer is already bounded by the worker.
func identifyMP4(data []byte, out *ResourceResult) {
	out.Format = "mp4"
	out.Signature = "iso_bmff_ftyp"
	tracks := []*mp4AudioTrack{}
	media := false
	boxes := 0
	_, err := mp4.ReadBoxStructure(bytes.NewReader(data), func(h *mp4.ReadHandle) (any, error) {
		boxes++
		if boxes > 4096 || len(h.Path) > 8 {
			return nil, errors.New("metadata traversal limit")
		}
		bi := h.BoxInfo
		if bi.Size < bi.HeaderSize || bi.Offset > uint64(len(data)) || bi.HeaderSize > uint64(len(data))-bi.Offset {
			return nil, errors.New("invalid box bounds")
		}
		name := bi.Type.String()
		if name == "mdat" {
			media = media || (bi.Size > bi.HeaderSize && uint64(len(data))-bi.Offset-bi.HeaderSize >= 16)
			return nil, nil
		}
		if bi.Size > uint64(len(data))-bi.Offset {
			return nil, errors.New("metadata outside read window")
		}
		var track *mp4AudioTrack
		if len(h.Params) > 0 {
			track, _ = h.Params[0].(*mp4AudioTrack)
		}
		switch strings.Join(boxNames(h.Path), "/") {
		case "moov":
			return h.Expand()
		case "moov/trak":
			if len(tracks) >= 32 {
				return nil, errors.New("track limit")
			}
			track = &mp4AudioTrack{}
			tracks = append(tracks, track)
			return h.Expand(track)
		case "moov/trak/mdia", "moov/trak/mdia/minf", "moov/trak/mdia/minf/stbl":
			return h.Expand(track)
		case "moov/trak/mdia/mdhd":
			if bi.Size-bi.HeaderSize > 48 {
				return nil, errors.New("invalid media header size")
			}
			payload, _, e := h.ReadPayload()
			if e != nil {
				return nil, e
			}
			mdhd := payload.(*mp4.Mdhd)
			track.timescale, track.duration = mdhd.Timescale, mdhd.GetDuration()
		case "moov/trak/mdia/hdlr":
			if bi.Size-bi.HeaderSize > 1024 {
				return nil, errors.New("handler header limit")
			}
			payload, _, e := h.ReadPayload()
			if e != nil {
				return nil, e
			}
			track.sound = payload.(*mp4.Hdlr).HandlerType == [4]byte{'s', 'o', 'u', 'n'}
		case "moov/trak/mdia/minf/stbl/stsd":
			payload, _, e := h.ReadPayload()
			if e != nil {
				return nil, e
			}
			if payload.(*mp4.Stsd).EntryCount == 0 {
				return nil, nil
			}
			if payload.(*mp4.Stsd).EntryCount > 16 {
				return nil, errors.New("sample description limit")
			}
			return h.Expand(track)
		case "moov/trak/mdia/minf/stbl/stsd/mp4a":
			if bi.Size-bi.HeaderSize > 64<<10 {
				return nil, errors.New("audio entry limit")
			}
			payload, _, e := h.ReadPayload()
			if e != nil {
				return nil, e
			}
			entry := payload.(*mp4.AudioSampleEntry)
			track.entry = entry.EntryVersion == 0 && entry.ChannelCount > 0 && entry.GetSampleRate() > 0
			track.codec = "mp4a"
			return h.Expand(track)
		case "moov/trak/mdia/minf/stbl/stsd/mp4a/esds":
			if bi.Size-bi.HeaderSize > 4096 {
				return nil, errors.New("decoder descriptor limit")
			}
			payload, _, e := h.ReadPayload()
			if e != nil {
				return nil, e
			}
			for _, descriptor := range payload.(*mp4.Esds).Descriptors {
				if config := descriptor.DecoderConfigDescriptor; config != nil && config.StreamType == 5 && config.ObjectTypeIndication == 0x40 {
					track.codec = "aac"
				}
			}
		}
		return nil, nil
	})
	if err != nil {
		out.ReasonCode = "mp4_metadata_invalid"
		out.Reason = "MP4 元数据解析未通过：" + safe(err)
		if err.Error() == "metadata outside read window" {
			out.ReasonCode = "mp4_metadata_outside_read_window"
			out.Reason = "MP4 元数据超出本次读取范围，证据不足"
		}
		return
	}
	for _, track := range tracks {
		if !track.sound || !track.entry || !media {
			continue
		}
		out.Status = "recognized_audio"
		out.Format = "m4a"
		out.Codec = track.codec
		if track.timescale > 0 && track.duration > 0 && track.duration != 0xffffffff && track.duration != 0xffffffffffffffff && track.duration/uint64(track.timescale) <= 86400 {
			ms := int64(track.duration/uint64(track.timescale)*1000 + track.duration%uint64(track.timescale)*1000/uint64(track.timescale))
			if ms > 0 {
				out.DurationMS = &ms
				out.DurationMethod = "mp4_mdhd"
			}
		}
		return
	}
	out.ReasonCode = "mp4_audio_evidence_missing"
	out.Reason = "已识别 MP4 容器，当前读取范围内缺少支持的音轨或媒体数据证据"
}

func boxNames(path mp4.BoxPath) []string {
	names := make([]string, len(path))
	for i, box := range path {
		names[i] = box.String()
	}
	return names
}
