package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"strings"
	"time"

	"cyapi-go/internal/auth"
)

const endpoint = "https://api.brevo.com/v3/smtp/email"

type Options struct {
	APIKey, SenderEmail, SenderName, AppName string
	Endpoint                                 string
	Client                                   *http.Client
}
type Brevo struct {
	opts   Options
	client *http.Client
}

func NewBrevo(o Options) *Brevo {
	if o.Endpoint == "" {
		o.Endpoint = endpoint
	}
	if o.AppName == "" {
		o.AppName = "CyreneMusic"
	}
	if o.SenderName == "" {
		o.SenderName = o.AppName
	}
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &Brevo{o, client}
}
func (b *Brevo) Ready() bool {
	return strings.TrimSpace(b.opts.APIKey) != "" && strings.TrimSpace(b.opts.SenderEmail) != ""
}

var bodyTemplate = template.Must(template.New("verification").Parse(`<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>{{.App}}</title><body><h2>{{.Title}}</h2><p>你好，{{.Username}}</p><p>验证码：<strong>{{.Code}}</strong></p><p>有效期 {{.Minutes}} 分钟。请勿向他人泄露验证码；如非本人操作，请忽略此邮件。</p></body></html>`))

func (b *Brevo) Send(ctx context.Context, m auth.Message) error {
	if !b.Ready() {
		return auth.ErrMailUnavailable
	}
	title := "注册验证码"
	if m.Purpose == "reset_password" {
		title = "重置密码验证码"
	}
	var html bytes.Buffer
	if err := bodyTemplate.Execute(&html, map[string]any{"App": b.opts.AppName, "Title": title, "Username": m.Username, "Code": m.Code, "Minutes": int(m.Expires / time.Minute)}); err != nil {
		return errors.New("email template failed")
	}
	payload := map[string]any{"sender": map[string]string{"email": b.opts.SenderEmail, "name": b.opts.SenderName}, "to": []map[string]string{{"email": m.To, "name": m.Username}}, "subject": b.opts.AppName + " " + title, "htmlContent": html.String()}
	data, err := json.Marshal(payload)
	if err != nil {
		return errors.New("email encoding failed")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.opts.Endpoint, bytes.NewReader(data))
	if err != nil {
		return auth.ErrMailDelivery
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("api-key", b.opts.APIKey)
	response, err := b.client.Do(request)
	if err != nil {
		return auth.ErrMailDelivery
	}
	defer response.Body.Close()
	// Drain only a bounded body; never return provider errors or secrets to clients/logs.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return auth.ErrMailDelivery
	}
	return nil
}
