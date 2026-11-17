package sourcecheck

import (
	"bytes"
	"context"
	"cyapi-go/internal/music"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func checkResource(ctx context.Context, client *http.Client, raw string, expected *int64) (out ResourceResult) {
	out = ResourceResult{Status: "unverified", Assessment: "unknown"}
	start := time.Now()
	defer func() { out.ElapsedMS = time.Since(start).Milliseconds() }()
	u, e := url.Parse(raw)
	if e != nil {
		out.Reason = "invalid URL"
		return
	}
	out.Host = u.Hostname()
	out.Extension = resourceExtension(u)
	req, e := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if e != nil {
		out.Reason = safe(e)
		return
	}
	req.Header.Set("Range", "bytes=0-8388607")
	req.Header.Set("Accept-Encoding", "identity")
	applyAudioHeaders(req)
	audioClient := resourceHTTPClient(client)
	resp, e := audioClient.Do(req)
	if e != nil {
		out.Status = "unreachable"
		out.Reason = safe(e)
		out.ReasonCode = "resource_request_failed"
		return
	}
	defer resp.Body.Close()
	out.HTTPStatus = resp.StatusCode
	if resp.Request != nil && resp.Request.URL != nil {
		out.FinalHost = resp.Request.URL.Hostname()
		if ext := resourceExtension(resp.Request.URL); ext != "" {
			out.Extension = ext
		}
	}
	out.ContentType, _, _ = mime.ParseMediaType(resp.Header.Get("Content-Type"))
	out.ContentRange = safe(resp.Header.Get("Content-Range"))
	out.ContentLength = resp.ContentLength
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		out.Status = "unreachable"
		out.ReasonCode = "resource_http_error"
		out.Reason = "音频资源返回 HTTP " + strconv.Itoa(resp.StatusCode) + " " + http.StatusText(resp.StatusCode)
		return
	}
	if resp.StatusCode == 206 && !strings.HasPrefix(resp.Header.Get("Content-Range"), "bytes 0-") {
		out.Reason = "unexpected partial content offset"
		out.ReasonCode = "unexpected_content_range"
		return
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		out.Reason = "unsupported audio content encoding"
		out.ReasonCode = "unsupported_content_encoding"
		return
	}
	data, stop, e := readResource(resp.Body, func(n int) error { return accountDecoded(client, n) }, out.ContentType)
	out.Bytes = len(data)
	out.ReadStop = stop
	out.ReadLimit = stop == "read_limit"
	if e != nil {
		out.Status = "unreachable"
		out.Reason = safe(e)
		out.ReasonCode = "resource_read_failed"
		return
	}
	out.Complete = stop == "eof" && completeAudioResponse(resp, len(data), out.ReadLimit)
	if len(data) > 0 {
		out.HeaderHex = hex.EncodeToString(data[:min(len(data), 16)])
	}
	identifyAudio(data, &out)
	explainUnknownResource(&out)
	if out.Complete {
		out.SHA256 = digest(data)
		if out.Format == "mp3" {
			if ms, ok := completeMP3Duration(data); ok {
				out.DurationMS = &ms
				out.DurationMethod = "mp3_complete_frames"
			}
		}
	}
	if out.DurationMS != nil && expected != nil {
		out.Assessment = music.AssessAudioDuration(*expected, *out.DurationMS)
	}
	return
}

// One response, one global cap. Successful audio keeps the full evidence path:
// hashes, complete MP3 frames and Ogg/MP4 tail metadata must not be skipped.
func readResource(body io.Reader, account func(int) error, contentType string) ([]byte, string, error) {
	var data bytes.Buffer
	chunk := make([]byte, 64<<10)
	for data.Len() < maxBytes {
		n, err := io.ReadFull(body, chunk[:min(len(chunk), maxBytes-data.Len())])
		data.Write(chunk[:n])
		if budgetErr := account(n); budgetErr != nil {
			return data.Bytes(), "read_error", budgetErr
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return data.Bytes(), "eof", nil
		}
		if err != nil {
			return data.Bytes(), "read_error", err
		}
		if nonAudioPrefix(data.Bytes(), contentType) {
			return data.Bytes(), "non_audio_response", nil
		}
	}
	return data.Bytes(), "read_limit", nil
}

func htmlPrefix(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	prefix := bytes.ToLower(trimmed[:min(len(trimmed), 14)])
	return bytes.HasPrefix(prefix, []byte("<!doctype html")) || bytes.HasPrefix(prefix, []byte("<html"))
}

func nonAudioPrefix(data []byte, contentType string) bool {
	return htmlPrefix(data) || ((contentType == "application/json" || strings.HasSuffix(contentType, "+json")) && json.Valid(bytes.TrimSpace(data)))
}

// Recognizing structure is deliberately weaker than decoding or verifying the song.
func identifyAudio(data []byte, out *ResourceResult) {
	if len(data) == 0 {
		out.Status = "invalid"
		out.Reason = "empty resource"
		out.ReasonCode = "empty_resource"
		return
	}
	trimmed := bytes.TrimSpace(data)
	if htmlPrefix(trimmed) || json.Valid(trimmed) {
		out.Status = "invalid"
		out.Reason = "resource is a text error response, not audio"
		out.Signature = "text_response"
		out.ReasonCode = "non_audio_response"
		return
	}
	if len(data) >= 4 && string(data[:4]) == "OggS" {
		identifyOgg(data, out)
		return
	}
	if len(data) >= 16 && string(data[4:8]) == "ftyp" {
		identifyMP4(data, out)
		return
	}
	if len(data) >= 42 && string(data[:4]) == "fLaC" && data[4]&127 == 0 && data[5] == 0 && data[6] == 0 && data[7] == 34 {
		packed := binary.BigEndian.Uint64(data[18:26])
		rate := int64(packed >> 44)
		samples := int64(packed & 0xfffffffff)
		if rate > 0 && samples > 0 {
			out.Status = "recognized_audio"
			out.Format = "flac"
			out.Codec = "flac"
			out.Signature = "flac_streaminfo"
			ms := samples * 1000 / rate
			out.DurationMS = &ms
			out.DurationMethod = "flac_streaminfo"
			return
		}
	}
	start := 0
	if len(data) >= 10 && string(data[:3]) == "ID3" {
		for _, v := range data[6:10] {
			if v&128 != 0 {
				out.Reason = "invalid ID3 size"
				return
			}
		}
		start = 10 + int(data[6])<<21 + int(data[7])<<14 + int(data[8])<<7 + int(data[9])
		if data[3] == 4 && data[5]&16 != 0 {
			start += 10
		}
	}
	for off := start; off+4 <= len(data) && off < start+4096; off++ {
		size, rate, samples, side := mp3Frame(data[off:])
		if size == 0 || off+size+4 > len(data) {
			continue
		}
		next, _, _, _ := mp3Frame(data[off+size:])
		if next == 0 {
			continue
		}
		out.Status = "recognized_audio"
		out.Format = "mp3"
		out.Codec = "mp3"
		out.Signature = "consecutive_mpeg_audio_frames"
		xing := off + 4 + side
		if data[off+1]&1 == 0 {
			xing += 2
		}
		var count uint32
		method := ""
		if xing+12 <= len(data) && (string(data[xing:xing+4]) == "Xing" || string(data[xing:xing+4]) == "Info") && binary.BigEndian.Uint32(data[xing+4:xing+8])&1 != 0 {
			count = binary.BigEndian.Uint32(data[xing+8 : xing+12])
			method = "mp3_xing"
		}
		vbri := off + 36
		if count == 0 && vbri+18 <= len(data) && string(data[vbri:vbri+4]) == "VBRI" {
			count = binary.BigEndian.Uint32(data[vbri+14 : vbri+18])
			method = "mp3_vbri"
		}
		if count > 0 && count < 10000000 {
			ms := int64(count) * int64(samples) * 1000 / int64(rate)
			out.DurationMS = &ms
			out.DurationMethod = method
		}
		return
	}
	out.Signature = "unknown"
	out.ReasonCode = "unsupported_or_unknown_format"
	out.Reason = "本次读取的数据未匹配已支持的音频结构；尚不能确认资源格式"
}

// MPEG layer III only. Require a second valid frame before calling it audio.
func mp3Frame(b []byte) (size, rate, samples, side int) {
	if len(b) < 4 || b[0] != 255 || b[1]&224 != 224 || b[1]&6 != 2 {
		return
	}
	version := (b[1] >> 3) & 3
	bi := (b[2] >> 4) & 15
	sr := (b[2] >> 2) & 3
	if version == 1 || bi == 0 || bi == 15 || sr == 3 {
		return
	}
	rates := [3]int{44100, 48000, 32000}
	rate = rates[sr]
	samples = 1152
	side = 32
	bits := [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	coefficient := 144000
	if version != 3 {
		bits = [16]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}
		rate /= 2
		samples = 576
		side = 17
		coefficient = 72000
		if version == 0 {
			rate /= 2
		}
	}
	if b[3]>>6 == 3 {
		if version == 3 {
			side = 17
		} else {
			side = 9
		}
	}
	size = coefficient*bits[bi]/rate + int((b[2]>>1)&1)
	return
}
