package sourcecheck

import (
	"net/url"
	"path"
	"strings"
)

// Never retain the signed path or query. Only a small, known extension is useful
// evidence; a URL extension alone does not establish format or encryption.
func resourceExtension(u *url.URL) string {
	ext := strings.ToLower(path.Ext(u.Path))
	switch ext {
	case ".mp3", ".flac", ".m4a", ".mp4", ".ogg", ".opus", ".wav", ".aac", ".mgg", ".mflac", ".mgg0", ".mflac0", ".qmc0", ".qmc3", ".qmcflac", ".qmcogg":
		return ext
	}
	return ""
}

func explainUnknownResource(out *ResourceResult) {
	if out.Status != "unverified" || out.ReasonCode != "unsupported_or_unknown_format" {
		return
	}
	switch out.Extension {
	case ".mgg", ".mflac", ".mgg0", ".mflac0", ".qmc0", ".qmc3", ".qmcflac", ".qmcogg":
		out.ReasonCode = "suspected_encrypted_resource"
		out.Reason = "资源后缀指向 QQ 加密格式，读取内容未识别为已支持的音频结构；疑似加密，尚未进行解码验证"
	default:
		if out.ReadLimit || !out.Complete {
			out.ReasonCode = "format_evidence_incomplete"
			out.Reason = "本次仅取得部分资源，当前读取范围内未识别出支持的音频结构；证据不足"
		} else {
			out.ReasonCode = "unsupported_or_unknown_format"
			out.Reason = "已读取完整资源，但未识别出支持的音频结构；可能为未支持格式或其他数据，尚未进行解码验证"
		}
	}
}

// Compare complete bytes within one script/platform/quality in one batch. The
// warning does not establish which song (if any) is wrong or change its outcome.
type duplicateResources struct {
	samples map[string]Sample
	groups  map[string][]int
}

func newDuplicateResources(samples []Sample) *duplicateResources {
	d := &duplicateResources{samples: make(map[string]Sample), groups: make(map[string][]int)}
	for _, sample := range samples {
		d.samples[sample.ID] = sample
	}
	return d
}

func (d *duplicateResources) add(results []Result, index int) {
	r := &results[index]
	if !r.Resource.Complete || r.Resource.SHA256 == "" || r.Resolution != "ok" {
		return
	}
	sample, ok := d.samples[r.SampleID]
	if !ok {
		return
	}
	key := r.ScriptPath + ":" + r.ScriptSHA256 + ":" + r.Platform + ":" + r.Quality + ":" + r.Resource.SHA256
	for _, previous := range d.groups[key] {
		other := &results[previous]
		otherSample := d.samples[other.SampleID]
		if sample.TrackID == otherSample.TrackID || sameSampleRecording(sample, otherSample) {
			continue
		}
		r.Resource.DuplicateSampleIDs = append(r.Resource.DuplicateSampleIDs, other.SampleID)
		other.Resource.DuplicateSampleIDs = append(other.Resource.DuplicateSampleIDs, r.SampleID)
	}
	d.groups[key] = append(d.groups[key], index)
}

func sameSampleRecording(a, b Sample) bool {
	normalize := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	artist := func(s Sample) string {
		if value := str(s.MusicInfo["singer"]); value != "" {
			return normalize(value)
		}
		return normalize(str(s.MusicInfo["artist"]))
	}
	return normalize(a.Title) != "" && normalize(a.Title) == normalize(b.Title) && artist(a) != "" && artist(a) == artist(b)
}
