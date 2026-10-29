package netease

import (
	"context"
	"crypto/aes"
	"crypto/md5"
	"cyapi-go/internal/music"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func eapi(path string, payload any) (string, error) {
	b, e := json.Marshal(payload)
	if e != nil {
		return "", e
	}
	text := string(b)
	hash := md5.Sum([]byte("nobody" + path + "use" + text + "md5forencrypt"))
	data := []byte(path + "-36cd479b6b5-" + text + "-36cd479b6b5-" + hex.EncodeToString(hash[:]))
	padding := aes.BlockSize - len(data)%aes.BlockSize
	for i := 0; i < padding; i++ {
		data = append(data, byte(padding))
	}
	cipher, e := aes.NewCipher([]byte("e82ckenh8dichen8"))
	if e != nil {
		return "", e
	}
	for i := 0; i < len(data); i += aes.BlockSize {
		cipher.Encrypt(data[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	return hex.EncodeToString(data), nil
}
func (c *Client) player(ctx context.Context, id, level string) (string, string, int64, time.Time, error) {
	ids, _ := json.Marshal([]string{id})
	payload := struct {
		IDs         string `json:"ids"`
		Level       string `json:"level"`
		EncodeType  string `json:"encodeType"`
		ImmerseType string `json:"immerseType,omitempty"`
	}{IDs: string(ids), Level: level, EncodeType: "flac"}
	if level == "sky" {
		payload.ImmerseType = "c51"
	}
	params, e := eapi("/api/song/enhance/player/url/v1", payload)
	if e != nil {
		return "", "", 0, time.Time{}, music.BadUpstream()
	}
	var out struct {
		Code int
		Data []struct {
			ID         int64
			URL        *string
			Level      string
			Size, Expi int64
		}
	}
	e = c.request(ctx, "https://interface3.music.163.com/eapi/song/enhance/player/url/v1", url.Values{"params": {params}}, &out)
	if e != nil {
		return "", "", 0, time.Time{}, e
	}
	if out.Code != 200 || len(out.Data) != 1 {
		return "", "", 0, time.Time{}, music.BadUpstream()
	}
	item := out.Data[0]
	if item.URL == nil || *item.URL == "" {
		return "", "", 0, time.Time{}, music.ErrUnavailable
	}
	parsed, e := url.Parse(*item.URL)
	if e != nil || parsed.User != nil || parsed.Port() != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || !validMediaHost(parsed.Hostname()) || item.ID != mustNumber(id) || item.Size < 0 || item.Size > music.MaxID {
		return "", "", 0, time.Time{}, music.BadUpstream()
	}
	parsed.Scheme = "https"
	expiry := time.Time{}
	if item.Expi > 0 && item.Expi <= 86400 {
		expiry = time.Now().Add(time.Duration(item.Expi) * time.Second)
	}
	return parsed.String(), item.Level, item.Size, expiry, nil
}
func mustNumber(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }
func validMediaHost(host string) bool {
	host = strings.ToLower(host)
	return host == "music.163.com" || strings.HasSuffix(host, ".music.163.com") || strings.HasSuffix(host, ".music.126.net")
}
func levelName(s string) string {
	switch s {
	case "standard":
		return "标准音质"
	case "exhigh":
		return "极高音质"
	case "lossless":
		return "无损音质"
	case "hires":
		return "Hires音质"
	case "sky":
		return "沉浸环绕声"
	case "jyeffect":
		return "高清环绕声"
	case "jymaster":
		return "超清母带"
	}
	return "未知音质"
}
func formatSize(size int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	value := float64(size)
	i := 0
	for value >= 1024 && i < len(units)-1 {
		value /= 1024
		i++
	}
	return strconv.FormatFloat(math.Round(value*100)/100, 'f', -1, 64) + " " + units[i]
}
