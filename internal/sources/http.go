package sources

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"sort"
	"time"

	"cyapi-go/internal/server"
)

type Handler struct {
	manager *Manager
	log     *slog.Logger
}

func NewHandler(m *Manager, log *slog.Logger) *Handler {
	return &Handler{m, log}
}
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /lx/sources", h.list)
	mux.HandleFunc("GET /lx/script", h.script)
}
func (h *Handler) failure(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "message": message})
}
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s, e := h.manager.Current()
	if e != nil {
		h.failure(w, 503, "暂时无法获取音源列表，请稍后重试")
		return
	}
	defer s.Close()
	out := []map[string]string{}
	for _, entry := range s.Manifest.Files {
		if !isScript(entry.Path) {
			continue
		}
		if e = checkContext(r.Context()); e != nil {
			return
		}
		f, e := s.Open(entry)
		if e != nil {
			h.failure(w, 503, "本地音源缺失或损坏，请等待重新同步")
			return
		}
		f.Close()
		dir := path.Dir(entry.Path)
		if dir == "." {
			dir = ""
		}
		name := path.Base(entry.Path)
		out = append(out, map[string]string{"path": entry.Path, "name": name[:len(name)-3], "directory": dir, "downloadUrl": server.RequestOrigin(r) + "/lx/script?path=" + url.QueryEscape(entry.Path)})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": 200, "sources": out})
}
func (h *Handler) script(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	query, e := url.ParseQuery(r.URL.RawQuery)
	paths := query["path"]
	if e != nil || len(paths) != 1 || !isScript(paths[0]) {
		h.failure(w, 400, "无效的音源路径")
		return
	}
	s, e := h.manager.Current()
	if e != nil {
		h.failure(w, 503, "暂时无法下载音源，请稍后重试")
		return
	}
	defer s.Close()
	index := sort.Search(len(s.Manifest.Files), func(i int) bool { return s.Manifest.Files[i].Path >= paths[0] })
	if index == len(s.Manifest.Files) || s.Manifest.Files[index].Path != paths[0] {
		h.failure(w, 404, "该候选音源已移除，请刷新列表")
		return
	}
	entry := s.Manifest.Files[index]
	f, e := s.Open(entry)
	if e != nil {
		h.failure(w, 503, "本地音源缺失或损坏，请等待重新同步")
		return
	}
	defer f.Close()
	digest, e := hashFile(r.Context(), f, entry.Size)
	if e != nil || digest != entry.SHA256 {
		h.failure(w, 503, "本地音源缺失或损坏，请等待重新同步")
		return
	}
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		h.failure(w, 503, "暂时无法下载音源，请稍后重试")
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("ETag", `"`+entry.SHA256+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	publishedAt, _ := time.Parse(timeFormat, s.Manifest.PublishedAt)
	http.ServeContent(w, r, path.Base(entry.Path), publishedAt, f)
}
