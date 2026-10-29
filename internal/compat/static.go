// Package compat preserves explicit unconfigured and placeholder contracts from the original application.
package compat

import (
	"cyapi-go/internal/server"
	"encoding/json"
	"net/http"
	"net/netip"
)

type response struct {
	status int
	body   string
}

var static = map[string]response{
	"GET /auth/linuxdo-status":             {200, `{"status":200,"data":{"enabled":false}}`},
	"POST /auth/linuxdo/login":             {400, `{"code":400,"message":"Linux Do 登录未配置"}`},
	"GET /version/latest":                  {200, `{"status":200,"data":{"version":"1.0.2","changelog":"- 支持安卓平台 \n - 修复了一些bug","force_update":false,"download_url":"https://github.com/Chuxin-Neko/chuxinneko_music/releases/latest"}}`},
	"GET /recommend/songs":                 {200, `{"code":200,"recommend":[]}`},
	"GET /recommend/resource":              {200, `{"code":200,"recommend":[]}`},
	"GET /recommend/for_you":               {200, `{"status":200,"data":{"dailySongs":[],"fm":[],"dailyPlaylists":[],"personalizedPlaylists":[],"radarPlaylists":[],"personalizedNewsongs":[]}}`},
	"GET /personal_fm":                     {200, `{"code":200,"data":[]}`},
	"POST /fm_trash":                       {200, `{"code":200,"message":"ok"}`},
	"GET /personalized":                    {200, `{"code":200,"result":[]}`},
	"GET /personalized/newsong":            {200, `{"code":200,"result":[]}`},
	"GET /netease/user/playlists":          {200, `{"code":200,"data":{"playlists":[]}}`},
	"GET /artist/detail":                   {200, `{"status":200,"data":{}}`},
	"GET /artist/desc":                     {200, `{"status":200,"briefDesc":"","introduction":[]}`},
	"GET /song/wiki/summary":               {200, `{"status":200,"data":{}}`},
	"GET /song/music/detail/get":           {200, `{"status":200,"data":{}}`},
	"GET /kuwo/search":                     {200, `{"status":200,"data":{"songs":[]}}`},
	"GET /kuwo/song":                       {200, `{"status":200,"song":{"name":"","artist":"","album":"","pic":"","url":"","duration":0,"lyric":""}}`},
	"GET /kuwo/playlist":                   {200, `{"status":200,"data":{"playlists":[]}}`},
	"GET /kugou/user/playlists":            {200, `{"code":200,"data":{"playlists":[]}}`},
	"GET /kugou/playlist/tracks":           {200, `{"code":200,"data":{"tracks":[]}}`},
	"GET /lyrics/kuwo":                     {501, `{"status":501,"data":{"state":"unsupported","retryable":false}}`},
	"GET /apple/search":                    {200, `{"status":200,"result":[]}`},
	"GET /apple/playlist":                  {200, `{"status":200,"data":{"playlists":[]}}`},
	"GET /apple/song":                      {200, `{"status":200,"id":"","name":"","pic":"","ar_name":"","al_name":"","level":"","size":"0","url":"","lyric":"","tlyric":""}`},
	"GET /apple/stream":                    {501, `{"status":501,"message":"Apple Music 流未配置"}`},
	"GET /config/public":                   {200, `{"status":200,"data":{"qq_group":{"enabled":false,"url":"","name":""},"announcement":{"enabled":false,"id":"","title":"","content":"","level":"info"}}}`},
	"GET /info":                            {200, `{"status":200,"message":"ok"}`},
	"GET /weather":                         {200, `{"status":200,"data":{"text":""}}`},
	"POST /music-taste/generate-sync":      {200, `{"code":200,"data":{"content":""}}`},
	"GET /audio-proxy/stream":              {501, `{"status":501,"message":"audio-proxy 未配置"}`},
	"GET /audio/proxy":                     {501, `{"status":501,"message":"audio-proxy 未配置"}`},
	"GET /url/{source}/{songId}/{quality}": {501, `{"status":501,"message":"url 解析未配置"}`},
	"POST /v1/parse":                       {501, `{"code":501,"message":"parse 未配置"}`},
}

func Register(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	public := map[string]bool{"GET /auth/linuxdo-status": true, "POST /auth/linuxdo/login": true, "GET /version/latest": true, "GET /config/public": true, "GET /info": true, "GET /weather": true}
	for pattern, value := range static {
		fn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Compatibility-Mode", "placeholder")
			write(w, value.status, json.RawMessage(value.body))
		})
		if !public[pattern] {
			fn = guard(fn)
		}
		mux.HandleFunc(pattern, fn)
	}
	for _, path := range []string{"/ip-location", "/ip-location/query"} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			ip := server.ClientIP(r.Context())
			if path == "/ip-location/query" {
				ip = r.URL.Query().Get("ip")
				if ip == "" {
					ip = "0.0.0.0"
				}
				if _, e := netip.ParseAddr(ip); e != nil {
					write(w, 400, map[string]any{"success": false, "message": "IP 无效"})
					return
				}
			}
			write(w, 200, map[string]any{"success": true, "ip": ip, "location": map[string]string{"country": "", "province": "", "city": "", "isp": "", "latitude": "", "longitude": ""}})
		})
	}
	mux.HandleFunc("POST /music-taste/generate", guard(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Compatibility-Mode", "placeholder")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}
func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
