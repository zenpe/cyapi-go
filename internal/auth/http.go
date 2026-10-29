package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Handler struct {
	service *Service
	ip      func(context.Context) string
	log     *slog.Logger
}

func NewHandler(s *Service, ip func(context.Context) string, log *slog.Logger) *Handler {
	return &Handler{s, ip, log}
}
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/registration-status", func(w http.ResponseWriter, r *http.Request) {
		h.reply(w, 200, map[string]any{"code": 200, "status": 200, "data": map[string]any{"enabled": h.service.RegistrationEnabled()}})
	})
	mux.HandleFunc("POST /auth/register/send-code", h.endpoint(h.sendRegister))
	mux.HandleFunc("POST /auth/register", h.endpoint(h.register))
	mux.HandleFunc("POST /auth/login", h.endpoint(h.login))
	mux.HandleFunc("POST /auth/reset-password/send-code", h.endpoint(h.sendReset))
	mux.HandleFunc("POST /auth/reset-password", h.endpoint(h.reset))
	mux.HandleFunc("GET /auth/validate-token", h.Protect(func(w http.ResponseWriter, r *http.Request) {
		h.reply(w, 200, map[string]any{"code": 200, "data": CurrentUser(r.Context()).Public()})
	}))
}

type userKey struct{}

func CurrentUser(ctx context.Context) *User { u, _ := ctx.Value(userKey{}).(*User); return u }
func (h *Handler) Protect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fields := strings.Fields(r.Header.Get("Authorization"))
		if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
			h.failure(w, ErrUnauthorized)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		user, err := h.service.Authenticate(ctx, fields[1])
		if err != nil {
			h.failure(w, err)
			return
		}
		next(w, r.WithContext(context.WithValue(ctx, userKey{}, user)))
	}
}
func (h *Handler) endpoint(fn func(context.Context, map[string]string, string) (map[string]any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		body, err := decode(w, r)
		if err != nil {
			h.failure(w, err)
			return
		}
		response, err := fn(ctx, body, h.ip(r.Context()))
		if err != nil {
			h.failure(w, err)
			return
		}
		h.reply(w, 200, response)
	}
}
func decode(w http.ResponseWriter, r *http.Request) (map[string]string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, ErrInvalid
	}
	result := map[string]string{}
	switch media {
	case "application/json":
		dec := json.NewDecoder(r.Body)
		if err = dec.Decode(&result); err != nil || result == nil {
			return nil, ErrInvalid
		}
		var extra any
		if err = dec.Decode(&extra); err != io.EOF {
			return nil, ErrInvalid
		}
	case "application/x-www-form-urlencoded":
		if err = r.ParseForm(); err != nil {
			return nil, ErrInvalid
		}
		for key, values := range r.PostForm {
			if len(values) != 1 {
				return nil, ErrInvalid
			}
			result[key] = values[0]
		}
	default:
		return nil, ErrInvalid
	}
	return result, nil
}
func (h *Handler) reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (h *Handler) failure(w http.ResponseWriter, err error) {
	status, message := 500, "服务暂时不可用，请稍后重试"
	var rate *RateError
	switch {
	case errors.As(err, &rate):
		w.Header().Set("Retry-After", strconv.Itoa(rate.RetryAfter))
		h.reply(w, 429, map[string]any{"code": 429, "message": "操作过于频繁，请稍后重试", "retryAfterSeconds": rate.RetryAfter})
		return
	case errors.Is(err, ErrInvalid):
		status, message = 400, "参数无效：用户名需为4-20位字母数字下划线，密码需至少8个字符且不超过72字节，注册使用5-11位QQ号邮箱"
	case errors.Is(err, ErrUnauthorized):
		status, message = 401, "账号或密码错误，或登录已过期"
	case errors.Is(err, ErrConflict):
		status, message = 400, "邮箱或用户名已被使用"
	case errors.Is(err, ErrCode):
		status, message = 400, "验证码无效或已过期"
	case errors.Is(err, ErrMailUnavailable):
		status, message = 503, "邮件服务暂不可用，请稍后重试"
	case errors.Is(err, ErrMailDelivery):
		status, message = 502, "验证码发送失败，请稍后重试"
	case errors.Is(err, context.DeadlineExceeded):
		status, message = 504, "请求超时，请稍后重试"
	default:
		h.log.Error("account request failed")
	}
	h.reply(w, status, map[string]any{"code": status, "message": message})
}
func (h *Handler) sendRegister(ctx context.Context, b map[string]string, ip string) (map[string]any, error) {
	if err := h.service.SendCode(ctx, b["email"], b["username"], "register", ip); err != nil {
		return nil, err
	}
	return map[string]any{"code": 200, "message": "验证码已发送到您的邮箱，请查收", "data": map[string]any{"email": normalizeEmail(b["email"]), "expiresIn": int(CodeLifetime / time.Second)}}, nil
}
func (h *Handler) register(ctx context.Context, b map[string]string, ip string) (map[string]any, error) {
	u, err := h.service.Register(ctx, b["email"], b["username"], b["password"], b["code"], ip)
	if err != nil {
		return nil, err
	}
	return map[string]any{"code": 200, "message": "注册成功", "data": map[string]any{"id": u.ID, "email": u.Email, "username": u.Username, "createdAt": u.CreatedAt}}, nil
}
func (h *Handler) login(ctx context.Context, b map[string]string, ip string) (map[string]any, error) {
	u, token, err := h.service.Login(ctx, b["account"], b["password"], ip)
	if err != nil {
		return nil, err
	}
	data := u.Public()
	data["token"] = token
	return map[string]any{"code": 200, "message": "登录成功", "data": data}, nil
}
func (h *Handler) sendReset(ctx context.Context, b map[string]string, ip string) (map[string]any, error) {
	if err := h.service.SendCode(ctx, b["email"], "", "reset_password", ip); err != nil {
		return nil, err
	}
	return map[string]any{"code": 200, "message": "如果该邮箱已注册，验证码将发送到您的邮箱", "data": map[string]any{"email": normalizeEmail(b["email"]), "expiresIn": int(CodeLifetime / time.Second)}}, nil
}
func (h *Handler) reset(ctx context.Context, b map[string]string, ip string) (map[string]any, error) {
	if err := h.service.ResetPassword(ctx, b["email"], b["newPassword"], b["code"], ip); err != nil {
		return nil, err
	}
	return map[string]any{"code": 200, "message": "密码重置成功，请使用新密码登录"}, nil
}
