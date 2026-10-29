// Package sources mirrors the approved GitHub Release ZIP into immutable local generations.
package sources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var ErrBusy = errors.New("source synchronization already running")
var ErrUnavailable = errors.New("source generation unavailable or damaged")
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var generationPattern = regexp.MustCompile(`^g-[0-9]{20}-[a-f0-9]{16}$`)

const Format = 1
const timeFormat = "2006-01-02T15:04:05.000000000Z"

type Options struct {
	Directory, Repo, Token                                 string
	AllowEmpty                                             bool
	MaxZipBytes, MaxFileBytes, MaxTotalBytes, ReserveBytes int64
	MaxFiles                                               int
	Client                                                 *http.Client
}

func DefaultOptions(directory string) Options {
	return Options{Directory: directory, Repo: "guoyue2010/lxmusic-", MaxZipBytes: 512 << 20, MaxFileBytes: 64 << 20, MaxTotalBytes: 2 << 30, MaxFiles: 10000, ReserveBytes: 1 << 30}
}

type Manager struct {
	o       Options
	client  *http.Client
	free    func(string) (uint64, error)
	now     func() time.Time
	replace func(string, string) error
}

func New(o Options) (*Manager, error) {
	if o.Directory == "" || !repoPattern.MatchString(o.Repo) || strings.Contains(o.Repo, "..") || o.MaxZipBytes < 1 || o.MaxFileBytes < 1 || o.MaxTotalBytes < 1 || o.ReserveBytes < 0 || o.MaxFiles < 1 || o.MaxFiles > 10000 || o.MaxZipBytes > 2<<30 || o.MaxTotalBytes > 8<<30 || o.MaxFileBytes > o.MaxTotalBytes || o.ReserveBytes > 1<<40 {
		return nil, errors.New("invalid source repository or limits")
	}
	c := &http.Client{Timeout: 2 * time.Minute}
	if o.Client != nil {
		*c = *o.Client
	}
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !allowedURL(r.URL.Scheme, r.URL.Host, r.URL.User != nil) {
			return errors.New("source redirect rejected")
		}
		if r.URL.Host != "api.github.com" {
			r.Header.Del("Authorization")
		}
		return nil
	}
	return &Manager{o: o, client: c, free: diskFree, now: time.Now, replace: replaceFile}, nil
}

type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type Manifest struct {
	Format                                                                   int    `json:"format"`
	Generation                                                               string `json:"generation"`
	Repo                                                                     string `json:"repo"`
	ReleaseID, AssetID                                                       int64
	Tag, AssetName, AssetUpdatedAt, ArchiveSHA256, ConfigSHA256, PublishedAt string
	Files                                                                    []File `json:"files"`
}
type pointer struct {
	Format     int    `json:"format"`
	Generation string `json:"generation"`
}
type Report struct {
	Generation      string `json:"generation"`
	Tag             string `json:"tag"`
	Files           int    `json:"files"`
	Unchanged       bool   `json:"unchanged"`
	DryRun          bool   `json:"dryRun"`
	CleanupWarnings int    `json:"cleanupWarnings,omitempty"`
}

func configHash(o Options) string {
	b, _ := json.Marshal(struct {
		Format                 int
		Repo, Selection, Names string
		AllowEmpty             bool
	}{Format, o.Repo, "latest-stable:first-zip:all-files:strip-common-directory", "gb18030:portable-paths:v1", o.AllowEmpty})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func validPath(name string) bool {
	if name == "" || len(name) > 1024 || !utf8.ValidString(name) || path.IsAbs(name) || strings.ContainsAny(name, `\<>:"|?*`) {
		return false
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || len(part) > 255 || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return false
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || regexpReserved(base) {
			return false
		}
	}
	return true
}
func regexpReserved(s string) bool {
	return len(s) == 4 && (strings.HasPrefix(s, "COM") || strings.HasPrefix(s, "LPT")) && s[3] >= '1' && s[3] <= '9'
}
func isScript(name string) bool {
	return validPath(name) && strings.HasSuffix(strings.ToLower(name), ".js") && len(path.Base(name)) > 3
}
func validDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && s == strings.ToLower(s)
}
func readJSON(r io.Reader, out any, limit int64) error {
	b, e := io.ReadAll(io.LimitReader(r, limit+1))
	if e != nil || int64(len(b)) > limit {
		return ErrUnavailable
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	if e = d.Decode(out); e != nil {
		return ErrUnavailable
	}
	if d.Decode(new(any)) != io.EOF {
		return ErrUnavailable
	}
	return nil
}

// Reject links at every component; Root additionally prevents escapes during path resolution.
func safeOpen(root *os.Root, name string) (*os.File, error) {
	checked := name
	if strings.HasPrefix(name, "files/") {
		checked = strings.TrimPrefix(name, "files/")
	}
	if !validPath(checked) {
		return nil, ErrUnavailable
	}
	parts := strings.Split(name, "/")
	for i := range parts {
		info, e := root.Lstat(strings.Join(parts[:i+1], "/"))
		if e != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrUnavailable
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, ErrUnavailable
		}
	}
	f, e := root.Open(name)
	if e != nil {
		return nil, ErrUnavailable
	}
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, ErrUnavailable
	}
	return f, nil
}
func checkContext(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if e := checkContext(r.ctx); e != nil {
		return 0, e
	}
	return r.r.Read(b)
}
