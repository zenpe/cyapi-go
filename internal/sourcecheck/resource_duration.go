package sourcecheck

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
)

// A successful read below the limit reached EOF. A 206 response must also cover
// the whole declared representation, not merely the requested prefix.
func completeAudioResponse(resp *http.Response, n int, limited bool) bool {
	if limited || n == 0 || (resp.ContentLength >= 0 && resp.ContentLength != int64(n)) {
		return false
	}
	if resp.StatusCode == http.StatusOK {
		return resp.Header.Get("Content-Range") == ""
	}
	if resp.StatusCode != http.StatusPartialContent {
		return false
	}
	value := strings.TrimPrefix(resp.Header.Get("Content-Range"), "bytes 0-")
	if value == resp.Header.Get("Content-Range") {
		return false
	}
	end, total, ok := strings.Cut(value, "/")
	if !ok {
		return false
	}
	last, e1 := strconv.ParseInt(end, 10, 64)
	size, e2 := strconv.ParseInt(total, 10, 64)
	return e1 == nil && e2 == nil && size == int64(n) && last == size-1
}

// Scan complete MPEG Layer III frames only. Headerless CBR files have no Xing
// or VBRI frame count. Unknown tails, truncated frames and rate changes leave
// duration unknown rather than converting a prefix into a full-song duration.
func completeMP3Duration(data []byte) (int64, bool) {
	start, end := 0, len(data)
	if end >= 10 && string(data[:3]) == "ID3" {
		for _, v := range data[6:10] {
			if v&128 != 0 {
				return 0, false
			}
		}
		start = 10 + int(data[6])<<21 + int(data[7])<<14 + int(data[8])<<7 + int(data[9])
		if data[3] == 4 && data[5]&16 != 0 {
			start += 10
		}
	}
	if end-start >= 128 && bytes.Equal(data[end-128:end-125], []byte("TAG")) {
		end -= 128
	}
	var samples int64
	frames, rate := 0, 0
	for off := start; off < end; {
		size, hz, count, _ := mp3Frame(data[off:end])
		if size == 0 || off+size > end || (rate != 0 && rate != hz) {
			return 0, false
		}
		rate = hz
		samples += int64(count)
		frames++
		off += size
	}
	if frames < 2 || rate == 0 {
		return 0, false
	}
	return samples * 1000 / int64(rate), true
}
