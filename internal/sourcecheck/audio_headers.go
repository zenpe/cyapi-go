package sourcecheck

import (
	"net/http"
	"strings"
)

// Match the client's audio transport, not the platform of the selected track.
// This policy applies only to audio resources, never script API requests.
func applyAudioHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Del("Referer")
	req.Header.Del("Origin")
	host := strings.ToLower(req.URL.Hostname())
	belongs := func(domain string) bool { return host == domain || strings.HasSuffix(host, "."+domain) }
	if belongs("qqmusic.qq.com") || belongs("music.tc.qq.com") {
		req.Header.Set("Referer", "https://y.qq.com/")
		req.Header.Set("Origin", "https://y.qq.com")
	} else if belongs("kugou.com") || belongs("kugou.cn") {
		req.Header.Set("Referer", "https://www.kugou.com/")
		req.Header.Set("Origin", "https://www.kugou.com")
	}
}

func resourceHTTPClient(client *http.Client) *http.Client {
	copy := *client
	copy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if client.CheckRedirect != nil {
			if err := client.CheckRedirect(req, via); err != nil {
				return err
			}
		} else if len(via) >= 10 {
			return http.ErrUseLastResponse
		}
		applyAudioHeaders(req)
		return nil
	}
	return &copy
}
