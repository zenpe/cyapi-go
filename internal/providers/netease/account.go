package netease

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
)

var ErrAccountExpired = errors.New("netease account expired")
var ErrAccountUnavailable = errors.New("netease account unavailable")

// AccountRequest shares the transport and concurrency budget, never a mutable
// cookie jar. Only fixed official paths are accepted; cookies never reach redirects.
func (c *Client) AccountRequest(ctx context.Context, cookie, path string, data map[string]any) (map[string]any, string, error) {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#\\\r\n") {
		return nil, "", ErrAccountUnavailable
	}
	csrf := ""
	reqCookie := &http.Request{Header: http.Header{"Cookie": []string{cookie}}}
	for _, v := range reqCookie.Cookies() {
		if v.Name == "__csrf" {
			csrf = v.Value
		}
	}
	payload := map[string]any{}
	for k, v := range data {
		payload[k] = v
	}
	payload["csrf_token"] = csrf
	form, e := weapi(payload)
	if e != nil {
		return nil, "", e
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	req, e := http.NewRequestWithContext(ctx, "POST", "https://music.163.com/weapi"+path+"?csrf_token="+url.QueryEscape(csrf), strings.NewReader(form.Encode()))
	if e != nil {
		return nil, "", ErrAccountUnavailable
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", "https://music.163.com/")
	req.Header.Set("Origin", "https://music.163.com")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, e := c.http.Do(req)
	if e != nil {
		return nil, "", ErrAccountUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return nil, "", ErrAccountExpired
	}
	if resp.StatusCode != 200 {
		return nil, "", ErrAccountUnavailable
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if e != nil || len(b) > 8<<20 {
		return nil, "", ErrAccountUnavailable
	}
	out := map[string]any{}
	if json.Unmarshal(b, &out) != nil {
		return nil, "", ErrAccountUnavailable
	}
	code, _ := out["code"].(float64)
	if code == 301 || code == 401 {
		return nil, "", ErrAccountExpired
	}
	var cookies []string
	for _, v := range resp.Cookies() {
		cookies = append(cookies, v.Name+"="+v.Value)
	}
	return out, strings.Join(cookies, "; "), nil
}

// The official weapi protocol uses two AES-CBC passes and raw RSA for the
// ephemeral AES key. This is a protocol implementation, not credential storage.
func weapi(data map[string]any) (url.Values, error) {
	b, e := json.Marshal(data)
	if e != nil {
		return nil, e
	}
	raw := make([]byte, 16)
	if _, e = rand.Read(raw); e != nil {
		return nil, e
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	key := make([]byte, 16)
	for i, v := range raw {
		key[i] = alphabet[int(v)%len(alphabet)]
	}
	encrypt := func(text, key []byte) string {
		block, _ := aes.NewCipher(key)
		pad := aes.BlockSize - len(text)%aes.BlockSize
		text = append(append([]byte{}, text...), bytes.Repeat([]byte{byte(pad)}, pad)...)
		cipher.NewCBCEncrypter(block, []byte("0102030405060708")).CryptBlocks(text, text)
		return base64.StdEncoding.EncodeToString(text)
	}
	first := encrypt(b, []byte("0CoJUm6Qyw8W8jud"))
	params := encrypt([]byte(first), key)
	block, _ := pem.Decode([]byte("-----BEGIN PUBLIC KEY-----\nMIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDgtQn2JZ34ZC28NWYpAUd98iZ37BUrX/aKzmFbt7clFSs6sXqHauqKWqdtLkF2KexO40H1YTX8z2lSgBBOAxLsvaklV8k4cBFK9snQXE9/DDaFt6Rr7iVZMldczhC0JNgTz+SHXT6CBHuX3e9SdB1Ua44oncaTWz7OBGLbCiK45wIDAQAB\n-----END PUBLIC KEY-----"))
	parsed, e := x509.ParsePKIXPublicKey(block.Bytes)
	if e != nil {
		return nil, e
	}
	// Extract RSA parameters through the standard key interface.
	pub := parsed.(*rsa.PublicKey)
	for i, j := 0, len(key)-1; i < j; i, j = i+1, j-1 {
		key[i], key[j] = key[j], key[i]
	}
	encrypted := new(big.Int).Exp(new(big.Int).SetBytes(key), big.NewInt(int64(pub.E)), pub.N)
	enc := hex.EncodeToString(encrypted.FillBytes(make([]byte, pub.Size())))
	return url.Values{"params": {params}, "encSecKey": {enc}}, nil
}

// WithAccountCookie returns an immutable view sharing the bounded transport.
func (c *Client) WithAccountCookie(cookie string) *Client {
	view := *c
	view.cookie = cookie
	return &view
}
