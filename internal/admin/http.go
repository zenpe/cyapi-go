// Package admin exposes authenticated source diagnostics and a small embedded UI.
package admin

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"cyapi-go/internal/auth"
	"cyapi-go/internal/sourcecheck"
	"cyapi-go/internal/sourcerecommendation"
	"cyapi-go/internal/sources"
)

//go:embed web/*
var assets embed.FS

type Handler struct {
	automatic             *sourcerecommendation.Automatic
	recommendations       *sourcerecommendation.Store
	recommendationEnabled bool
	service               *sourcecheck.Service
	controller            *sourcecheck.Controller
	sources               *sources.Manager
	guard                 func(http.HandlerFunc) http.HandlerFunc
	userID                func(*http.Request) int64
	allowed               map[int64]bool
	metadata              sourcecheck.MetadataLoader
	log                   *slog.Logger
}

func New(s *sourcecheck.Service, c *sourcecheck.Controller, m *sources.Manager, guard func(http.HandlerFunc) http.HandlerFunc, ids []int64, metadata sourcecheck.MetadataLoader, log *slog.Logger) *Handler {
	allowed := map[int64]bool{}
	for _, id := range ids {
		allowed[id] = true
	}
	return &Handler{service: s, controller: c, sources: m, guard: guard, allowed: allowed, metadata: metadata, log: log, userID: func(r *http.Request) int64 {
		u := auth.CurrentUser(r.Context())
		if u == nil {
			return 0
		}
		return u.ID
	}}
}
func (h *Handler) WithRecommendations(s *sourcerecommendation.Store, enabled bool) *Handler {
	h.recommendations = s
	h.recommendationEnabled = enabled
	return h
}
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/source-management", h.page)
	mux.HandleFunc("GET /admin/source-management/assets/{file}", h.asset)
	routes := map[string]http.HandlerFunc{
		"GET /admin/source-checks/overview":          h.overview,
		"GET /admin/source-checks/samples":           h.samples,
		"POST /admin/source-checks/samples":          h.addSample,
		"PATCH /admin/source-checks/samples/{id}":    h.editSample,
		"DELETE /admin/source-checks/samples/{id}":   h.deleteSample,
		"POST /admin/source-checks/runs":             h.start,
		"GET /admin/source-checks/runs/{id}":         h.report,
		"POST /admin/source-checks/runs/{id}/cancel": h.cancel,
	}
	if h.recommendations != nil {
		routes["GET /admin/source-recommendations"] = h.recommendationHistory
		routes["POST /admin/source-recommendations"] = h.publishRecommendation
	}
	if h.automatic != nil {
		h.automaticRoutes(routes)
	}
	for route, fn := range routes {
		mux.HandleFunc(route, h.guard(h.authorize(fn)))
	}
}
func (h *Handler) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.allowed[h.userID(r)] {
			reply(w, 403, map[string]any{"message": "当前账号没有音源管理权限"})
			return
		}
		next(w, r)
	}
}
func reply(w http.ResponseWriter, status int, value map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	value["status"] = status
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (h *Handler) failure(w http.ResponseWriter, e error) {
	status, message := 500, "操作失败，请查看服务日志"
	switch {
	case errors.Is(e, sourcecheck.ErrBusy):
		status, message = 409, "已有检测或文件操作正在进行，请稍后重试"
	case errors.Is(e, sourcecheck.ErrEnvironment):
		status, message = 503, "检测环境未就绪：Linux 服务需要 cgroup v2 内存委派，请更新服务配置后重启；本次未执行音源检测"
		h.log.Error("source check environment unavailable", "error", e.Error())
	case errors.Is(e, sourcerecommendation.ErrConflict):
		status, message = 409, "推荐或音源版本已改变，请刷新后重试"
	case errors.Is(e, sourcerecommendation.ErrInvalid):
		status, message = 400, "发布参数无效，请填写说明并确认目标版本"
	case errors.Is(e, sourcecheck.ErrConflict):
		status, message = 409, "样例已被修改，请刷新后重试"
	case errors.Is(e, sourcecheck.ErrNotFound):
		status, message = 404, "记录不存在或已清理"
	case errors.Is(e, sourcecheck.ErrInvalid):
		status, message = 400, "参数无效，请检查歌曲平台、ID 和操作内容"
	case errors.Is(e, context.DeadlineExceeded):
		status, message = 504, "请求超时，请稍后重试"
	case errors.Is(e, context.Canceled):
		status, message = 503, "服务正在停止"
	default:
		h.log.Error("source administration failed", "error", e.Error())
	}
	reply(w, status, map[string]any{"message": message})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	media, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || media != "application/json" {
		return sourcecheck.ErrInvalid
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return sourcecheck.ErrInvalid
	}
	if d.Decode(new(any)) != io.EOF {
		return sourcecheck.ErrInvalid
	}
	return nil
}
func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	h.static(w, r, "index.html", "text/html; charset=utf-8")
}
func (h *Handler) asset(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	switch file {
	case "app.js":
		h.static(w, r, file, "text/javascript; charset=utf-8")
	case "style.css":
		h.static(w, r, file, "text/css; charset=utf-8")
	default:
		http.NotFound(w, r)
	}
}
func (h *Handler) static(w http.ResponseWriter, r *http.Request, file, contentType string) {
	b, e := assets.ReadFile("web/" + file)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	_, _ = w.Write(b)
}
func (h *Handler) samples(w http.ResponseWriter, r *http.Request) {
	set, version, e := h.service.Samples(r.Context())
	if e != nil {
		h.failure(w, e)
		return
	}
	reply(w, 200, map[string]any{"samples": set.Samples, "version": version})
}

type sampleInput struct {
	Platform        string `json:"platform"`
	TrackID         string `json:"trackId"`
	ExpectedVersion string `json:"expectedVersion"`
	Enabled         *bool  `json:"enabled,omitempty"`
	Action          string `json:"action,omitempty"`
}

func (h *Handler) fetch(w http.ResponseWriter, r *http.Request, platform, id string) (sourcecheck.Sample, bool) {
	if (platform != "qq" && platform != "netease") || id == "" || len(id) > 64 {
		h.failure(w, sourcecheck.ErrInvalid)
		return sourcecheck.Sample{}, false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || platform == "qq" && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z')) {
			h.failure(w, sourcecheck.ErrInvalid)
			return sourcecheck.Sample{}, false
		}
	}
	m, e := h.metadata(r.Context(), platform, id)
	if e != nil {
		h.log.Warn("sample metadata unavailable", "platform", platform, "trackId", id, "errorType", fmt.Sprintf("%T", e))
		reply(w, 502, map[string]any{"message": "无法获取歌曲详情，样例未修改；请检查歌曲 ID 后重试"})
		return sourcecheck.Sample{}, false
	}
	if m.TrackID != id {
		reply(w, 502, map[string]any{"message": "歌曲详情与请求 ID 不一致，样例未修改"})
		return sourcecheck.Sample{}, false
	}
	sample, e := sourcecheck.SampleFromMetadata(platform, m)
	if e != nil {
		reply(w, 502, map[string]any{"message": "歌曲详情不完整，样例未修改"})
		return sample, false
	}
	return sample, true
}
func (h *Handler) addSample(w http.ResponseWriter, r *http.Request) {
	var in sampleInput
	if e := decode(w, r, &in); e != nil {
		h.failure(w, e)
		return
	}
	if in.ExpectedVersion == "" || in.Action != "" || in.Enabled != nil {
		h.failure(w, sourcecheck.ErrInvalid)
		return
	}
	sample, ok := h.fetch(w, r, in.Platform, strings.TrimSpace(in.TrackID))
	if !ok {
		return
	}
	set, version, e := h.service.ChangeSamples(r.Context(), in.ExpectedVersion, func(set *sourcecheck.SampleSet) error {
		for _, s := range set.Samples {
			if s.ID == sample.ID {
				return sourcecheck.ErrConflict
			}
		}
		set.Samples = append(set.Samples, sample)
		return nil
	})
	if e != nil {
		h.failure(w, e)
		return
	}
	h.log.Info("source sample added", "actorId", h.userID(r), "sampleId", sample.ID)
	reply(w, 201, map[string]any{"samples": set.Samples, "version": version})
}
func (h *Handler) editSample(w http.ResponseWriter, r *http.Request) {
	var in sampleInput
	if e := decode(w, r, &in); e != nil {
		h.failure(w, e)
		return
	}
	if in.Platform != "" || in.TrackID != "" || ((in.Action != "refresh" || in.Enabled != nil) && (in.Action != "" || in.Enabled == nil)) {
		h.failure(w, sourcecheck.ErrInvalid)
		return
	}
	id := r.PathValue("id")
	var replacement sourcecheck.Sample
	if in.Action == "refresh" {
		set, _, e := h.service.Samples(r.Context())
		if e != nil {
			h.failure(w, e)
			return
		}
		found := false
		for _, s := range set.Samples {
			if s.ID == id {
				var ok bool
				replacement, ok = h.fetch(w, r, s.Platform, s.TrackID)
				if !ok {
					return
				}
				found = true
				break
			}
		}
		if !found {
			h.failure(w, sourcecheck.ErrNotFound)
			return
		}
	}
	set, version, e := h.service.ChangeSamples(r.Context(), in.ExpectedVersion, func(set *sourcecheck.SampleSet) error {
		for i, s := range set.Samples {
			if s.ID == id {
				if in.Action == "refresh" {
					replacement.Enabled = s.Enabled
					set.Samples[i] = replacement
				} else {
					set.Samples[i].Enabled = *in.Enabled
				}
				return nil
			}
		}
		return sourcecheck.ErrNotFound
	})
	if e != nil {
		h.failure(w, e)
		return
	}
	reply(w, 200, map[string]any{"samples": set.Samples, "version": version})
}
func (h *Handler) deleteSample(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ExpectedVersion string `json:"expectedVersion"`
	}
	if e := decode(w, r, &in); e != nil {
		h.failure(w, e)
		return
	}
	id := r.PathValue("id")
	set, version, e := h.service.ChangeSamples(r.Context(), in.ExpectedVersion, func(set *sourcecheck.SampleSet) error {
		for i, s := range set.Samples {
			if s.ID == id {
				set.Samples = append(set.Samples[:i], set.Samples[i+1:]...)
				return nil
			}
		}
		return sourcecheck.ErrNotFound
	})
	if e != nil {
		h.failure(w, e)
		return
	}
	h.log.Info("source sample deleted", "actorId", h.userID(r), "sampleId", id)
	reply(w, 200, map[string]any{"samples": set.Samples, "version": version})
}
func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path     string `json:"path"`
		Platform string `json:"platform"`
	}
	if e := decode(w, r, &in); e != nil {
		h.failure(w, e)
		return
	}
	if len(in.Path) > 1024 || (in.Platform != "" && in.Platform != "qq" && in.Platform != "netease") {
		h.failure(w, sourcecheck.ErrInvalid)
		return
	}
	id, reused, e := h.controller.Start(r.Context(), sourcecheck.RunOptions{Path: in.Path, Platform: in.Platform})
	if e != nil {
		h.failure(w, e)
		return
	}
	h.log.Info("source check started via admin", "actorId", h.userID(r), "runId", id, "reused", reused)
	reply(w, 202, map[string]any{"runId": id, "reused": reused})
}
func (h *Handler) report(w http.ResponseWriter, r *http.Request) {
	report, e := h.service.Report(r.PathValue("id"))
	if e != nil {
		h.failure(w, e)
		return
	}
	reply(w, 200, map[string]any{"report": report, "currentProbeVersion": sourcecheck.ProbeVersion})
}
func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	if e := h.controller.Cancel(r.PathValue("id")); e != nil {
		h.failure(w, e)
		return
	}
	reply(w, 200, map[string]any{"message": "取消请求已受理"})
}

type platformHealth struct {
	Recognized, Short, Failed, Unverified, Unsupported, Canceled, Limited, Duplicate, SuspectedEncrypted int
	CheckedAt                                                                                            time.Time
	RunID                                                                                                string
	Stale                                                                                                bool
}
type sourceRow struct {
	Generation string                     `json:"generation"`
	Path       string                     `json:"path"`
	Name       string                     `json:"name"`
	SHA256     string                     `json:"sha256"`
	Platforms  map[string]*platformHealth `json:"platforms"`
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	_, sampleVersion, e := h.service.Samples(r.Context())
	if e != nil {
		h.failure(w, e)
		return
	}
	reports, e := h.service.Reports(20)
	if e != nil {
		h.failure(w, e)
		return
	}
	rows := []sourceRow{}
	warning := ""
	snap, e := h.sources.Current()
	if e != nil {
		warning = "当前音源列表不可用，请先完成音源同步"
	} else {
		for _, f := range snap.Manifest.Files {
			if strings.HasSuffix(strings.ToLower(f.Path), ".js") {
				name := f.Path
				if at := strings.LastIndex(name, "/"); at >= 0 {
					name = name[at+1:]
				}
				rows = append(rows, sourceRow{Generation: snap.Manifest.Generation, Path: f.Path, Name: strings.TrimSuffix(name, ".js"), SHA256: f.SHA256, Platforms: map[string]*platformHealth{}})
			}
		}
		snap.Close()
	}
	lookup := map[string]*sourceRow{}
	for i := range rows {
		lookup[rows[i].Path+":"+rows[i].SHA256] = &rows[i]
	}
	seen := map[string]bool{}
	history := []sourcecheck.RunSummary{}
	for _, report := range reports {
		history = append(history, sourcecheck.RunSummary{RunID: report.RunID, State: report.State, StartedAt: report.StartedAt, Completed: report.Completed, Planned: report.Planned, Error: report.Error})
		if report.SampleSetVersion != sampleVersion || report.ProbeVersion != sourcecheck.ProbeVersion {
			continue
		}
		for _, x := range report.Results {
			key := x.ScriptPath + ":" + x.ScriptSHA256
			row := lookup[key]
			if row == nil || seen[key+":"+x.Platform+":"+x.SampleID] {
				continue
			}
			seen[key+":"+x.Platform+":"+x.SampleID] = true
			health := row.Platforms[x.Platform]
			if health == nil {
				health = &platformHealth{CheckedAt: x.CheckedAt, RunID: report.RunID}
				row.Platforms[x.Platform] = health
			}
			if x.CheckedAt.Before(health.CheckedAt) {
				health.CheckedAt = x.CheckedAt
			}
			health.Stale = time.Since(health.CheckedAt) > 24*time.Hour
			if x.Resource.ReasonCode == "suspected_encrypted_resource" {
				health.SuspectedEncrypted++
			}
			if len(x.Resource.DuplicateSampleIDs) > 0 {
				health.Duplicate++
			}
			category := sourcecheck.ClassifyResult(x)
			if category == sourcecheck.OutcomeFailed && sourcecheck.ProbeLimited(x) {
				health.Limited++
			}
			switch category {
			case sourcecheck.OutcomeUnsupported:
				health.Unsupported++
			case sourcecheck.OutcomeCanceled:
				health.Canceled++
			case sourcecheck.OutcomeShort:
				health.Short++
			case sourcecheck.OutcomeRecognized:
				health.Recognized++
			case sourcecheck.OutcomeUnverified:
				health.Unverified++
			default:
				health.Failed++
			}
		}
	}
	reply(w, 200, map[string]any{"sources": rows, "runs": history, "activeRunId": h.controller.ActiveID(), "warning": warning, "sampleVersion": sampleVersion})
}

func (h *Handler) recommendationHistory(w http.ResponseWriter, r *http.Request) {
	current, history, e := h.recommendations.History(r.Context())
	if e != nil {
		h.failure(w, e)
		return
	}
	var descriptor any
	if current.ReleaseID != "" {
		descriptor = h.recommendations.Describe(current)
	}
	reply(w, 200, map[string]any{"current": current, "history": history, "recommendation": descriptor, "enabled": h.recommendationEnabled})
}
func (h *Handler) publishRecommendation(w http.ResponseWriter, r *http.Request) {
	var in sourcerecommendation.PublishInput
	if e := decode(w, r, &in); e != nil {
		h.failure(w, e)
		return
	}
	release, reused, e := h.recommendations.Publish(r.Context(), in, h.userID(r))
	if e != nil {
		h.failure(w, e)
		return
	}
	h.log.Info("source recommendation published", "releaseId", release.ReleaseID, "scriptSha256", release.ScriptSHA256, "actorId", h.userID(r), "reused", reused)
	reply(w, 200, map[string]any{"release": release, "reused": reused})
}
