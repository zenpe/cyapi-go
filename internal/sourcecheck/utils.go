package sourcecheck

import (
	"bytes"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strings"
)

const maxBytes = 8 << 20

var urlRE = regexp.MustCompile(`https?://[^\s"<>]+`)
var secretRE = regexp.MustCompile(`(?i)((?:api[_-]?key|token|cookie|authorization|password|secret|signature|sign|key)["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^&\s,;}]+)`)

func safe(v any) string {
	s := urlRE.ReplaceAllString(fmt.Sprint(v), "<url>")
	s = secretRE.ReplaceAllString(s, "${1}<redacted>")
	if len(s) > 900 {
		s = s[:900] + "..."
	}
	return strings.ToValidUTF8(s, "�")
}
func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
func num(v any) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case int:
		return float64(n)
	case float64:
		return n
	case json.Number:
		v, _ := n.Float64()
		return v
	}
	return 0
}
func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}
func jsJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func utility(a map[string]any) (result map[string]any) {
	defer func() {
		if recover() != nil {
			result = map[string]any{"error": "invalid utility arguments"}
		}
	}()
	decode := func(k string) []byte {
		b, e := base64.StdEncoding.DecodeString(str(a[k]))
		if e != nil || len(b) > maxBytes {
			panic("bytes")
		}
		return b
	}
	var out []byte
	switch str(a["operation"]) {
	case "random":
		n := int(num(a["size"]))
		if n < 0 || n > 65536 {
			panic("size")
		}
		out = make([]byte, n)
		if _, e := rand.Read(out); e != nil {
			panic(e)
		}
	case "aes":
		mode := str(a["mode"])
		key, iv, data := decode("key"), decode("iv"), decode("data")
		if mode != fmt.Sprintf("aes-%d-cbc", len(key)*8) && mode != fmt.Sprintf("aes-%d-ecb", len(key)*8) {
			panic("mode")
		}
		block, e := aes.NewCipher(key)
		if e != nil {
			panic(e)
		}
		padding := aes.BlockSize - len(data)%aes.BlockSize
		data = append(data, bytes.Repeat([]byte{byte(padding)}, padding)...)
		out = make([]byte, len(data))
		if strings.HasSuffix(mode, "cbc") {
			cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, data)
		} else {
			if len(iv) != 0 {
				panic("iv")
			}
			for i := 0; i < len(data); i += 16 {
				block.Encrypt(out[i:i+16], data[i:i+16])
			}
		}
	case "rsa":
		p, _ := pem.Decode([]byte(str(a["key"])))
		if p == nil {
			panic("pem")
		}
		var key *rsa.PublicKey
		if p.Type == "RSA PUBLIC KEY" {
			key, _ = x509.ParsePKCS1PublicKey(p.Bytes)
		} else {
			k, e := x509.ParsePKIXPublicKey(p.Bytes)
			if e != nil {
				panic(e)
			}
			key, _ = k.(*rsa.PublicKey)
		}
		if key == nil || key.N.BitLen() != 1024 {
			panic("key")
		}
		data := decode("data")
		if len(data) > 128 {
			panic("length")
		}
		n := new(big.Int).SetBytes(data)
		if n.Cmp(key.N) >= 0 {
			panic("block")
		}
		out = new(big.Int).Exp(n, big.NewInt(int64(key.E)), key.N).FillBytes(make([]byte, 128))
	case "inflate":
		r, e := zlib.NewReader(bytes.NewReader(decode("data")))
		if e != nil {
			panic(e)
		}
		defer r.Close()
		out, e = io.ReadAll(io.LimitReader(r, maxBytes+1))
		if e != nil || len(out) > maxBytes {
			panic("inflate")
		}
	case "deflate":
		var b bytes.Buffer
		w := zlib.NewWriter(&b)
		w.Write(decode("data"))
		w.Close()
		out = b.Bytes()
	default:
		panic("operation")
	}
	return map[string]any{"value": base64.StdEncoding.EncodeToString(out)}
}
func annotation(raw, key string) string {
	re := regexp.MustCompile(`(?m)\*\s*@` + key + `\s+([^\r\n]+)`)
	m := re.FindStringSubmatch(raw)
	if len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}
