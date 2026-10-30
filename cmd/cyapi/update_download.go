package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const updateRepository = "zenpe/cyapi-go"
const maxUpdateBytes = 128 << 20

type updateAsset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}
type updateRelease struct {
	Tag        string        `json:"tag_name"`
	Draft      bool          `json:"draft"`
	Prerelease bool          `json:"prerelease"`
	Assets     []updateAsset `json:"assets"`
}
type releaseUpdater struct {
	client       *http.Client
	apiBase      string
	downloadBase string
	token        string
}

var newReleaseUpdater = githubUpdater

func githubUpdater(token string) releaseUpdater {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 15 * time.Second
	transport.ResponseHeaderTimeout = 30 * time.Second
	return releaseUpdater{
		client: &http.Client{Transport: transport, Timeout: 5 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many download redirects")
			}
			if req.URL.Scheme != "https" {
				return errors.New("download redirect must use HTTPS")
			}
			if len(via) > 0 && req.URL.Host != via[0].URL.Host {
				req.Header.Del("Authorization")
			}
			return nil
		}},
		apiBase:      "https://api.github.com/repos/" + updateRepository,
		downloadBase: "https://github.com/" + updateRepository + "/releases/download",
		token:        token,
	}
}

var stableVersionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func stableVersion(value string) ([3]uint64, error) {
	var out [3]uint64
	matches := stableVersionPattern.FindStringSubmatch(value)
	if matches == nil {
		return out, fmt.Errorf("expected a stable vX.Y.Z version, got %q", value)
	}
	for i := range out {
		n, e := strconv.ParseUint(matches[i+1], 10, 64)
		if e != nil {
			return out, errors.New("release version exceeds supported range")
		}
		out[i] = n
	}
	return out, nil
}
func compareStable(a, b string) (int, error) {
	x, e := stableVersion(a)
	if e != nil {
		return 0, e
	}
	y, e := stableVersion(b)
	if e != nil {
		return 0, e
	}
	for i := range x {
		if x[i] < y[i] {
			return -1, nil
		}
		if x[i] > y[i] {
			return 1, nil
		}
	}
	return 0, nil
}
func (u releaseUpdater) get(ctx context.Context, address string, limit int64, api bool, destination io.Writer) error {
	request, e := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if e != nil {
		return e
	}
	request.Header.Set("User-Agent", "cyapi-upgrade/"+version)
	if api {
		request.Header.Set("Accept", "application/vnd.github+json")
		if u.token != "" {
			request.Header.Set("Authorization", "Bearer "+u.token)
		}
	}
	response, e := u.client.Do(request)
	if e != nil {
		return errors.New("GitHub connection failed or timed out")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > limit {
		return errors.New("release download exceeds size limit")
	}
	n, e := io.Copy(destination, io.LimitReader(response.Body, limit+1))
	if e != nil {
		return errors.New("release download interrupted")
	}
	if n > limit {
		return errors.New("release download exceeds size limit")
	}
	return nil
}
func (u releaseUpdater) latest(ctx context.Context) (updateRelease, error) {
	var release updateRelease
	var body strings.Builder
	if e := u.get(ctx, u.apiBase+"/releases/latest", 1<<20, true, &body); e != nil {
		return release, e
	}
	if e := json.Unmarshal([]byte(body.String()), &release); e != nil {
		return release, errors.New("invalid GitHub release metadata")
	}
	if release.Draft || release.Prerelease {
		return release, errors.New("latest release is not a published stable version")
	}
	if _, e := stableVersion(release.Tag); e != nil {
		return release, e
	}
	return release, nil
}
func checksumFor(data, name string) (string, error) {
	found := ""
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		digest, e := hex.DecodeString(fields[0])
		if e != nil || len(digest) != sha256.Size {
			return "", errors.New("invalid SHA256SUMS digest")
		}
		if found != "" {
			return "", errors.New("duplicate binary checksum in SHA256SUMS")
		}
		found = strings.ToLower(fields[0])
	}
	if found == "" {
		return "", fmt.Errorf("SHA256SUMS has no checksum for %s", name)
	}
	return found, nil
}

// downloadLatest only reads GitHub and stages a verified file. No service is stopped here.
func (u releaseUpdater) downloadLatest(ctx context.Context, root, installed, arch string) (file, digest, tag string, err error) {
	release, e := u.latest(ctx)
	if e != nil {
		return "", "", "", e
	}
	comparison, e := compareStable(installed, release.Tag)
	if e != nil {
		return "", "", "", fmt.Errorf("cannot compare installed version; use --file for development/prerelease builds: %w", e)
	}
	if comparison >= 0 {
		fmt.Println("Already up to date:", installed, "(latest stable:", release.Tag+")")
		return "", "", release.Tag, nil
	}
	if arch != "amd64" && arch != "arm64" {
		return "", "", "", errors.New("automatic updates support Linux amd64/arm64")
	}
	name := "cyapi-linux-" + arch
	var binary updateAsset
	seen := map[string]bool{}
	for _, asset := range release.Assets {
		if asset.Name != name && asset.Name != "SHA256SUMS" {
			continue
		}
		if seen[asset.Name] {
			return "", "", "", errors.New("duplicate release asset")
		}
		seen[asset.Name] = true
		if asset.Name == name {
			binary = asset
			if asset.Size <= 0 || asset.Size > maxUpdateBytes {
				return "", "", "", errors.New("invalid release binary size")
			}
		}
		if asset.Name == "SHA256SUMS" && (asset.Size <= 0 || asset.Size > 65536) {
			return "", "", "", errors.New("invalid checksum asset size")
		}
	}
	if !seen[name] || !seen["SHA256SUMS"] {
		return "", "", "", errors.New("latest release has no matching binary or SHA256SUMS")
	}
	base := strings.TrimRight(u.downloadBase, "/") + "/" + url.PathEscape(release.Tag) + "/"
	var sums strings.Builder
	if e = u.get(ctx, base+"SHA256SUMS", 65536, false, &sums); e != nil {
		return "", "", "", fmt.Errorf("download checksums: %w", e)
	}
	digest, e = checksumFor(sums.String(), name)
	if e != nil {
		return "", "", "", e
	}
	if binary.Digest != "" && binary.Digest != "sha256:"+digest {
		return "", "", "", errors.New("GitHub asset digest conflicts with SHA256SUMS")
	}
	output, e := os.CreateTemp(root, ".cyapi-download-")
	if e != nil {
		return "", "", "", e
	}
	file = output.Name()
	defer func() {
		output.Close()
		if err != nil {
			os.Remove(file)
		}
	}()
	fmt.Println("Downloading", release.Tag, "for linux/"+arch+"...")
	hash := sha256.New()
	if e = u.get(ctx, base+name, maxUpdateBytes, false, io.MultiWriter(output, hash)); e != nil {
		return file, "", "", fmt.Errorf("download binary: %w", e)
	}
	info, e := output.Stat()
	if e != nil {
		return file, "", "", e
	}
	if info.Size() != binary.Size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return file, "", "", errors.New("downloaded binary size or SHA256 checksum does not match")
	}
	if e = output.Chmod(0755); e != nil {
		return file, "", "", e
	}
	if e = output.Sync(); e != nil {
		return file, "", "", e
	}
	if e = output.Close(); e != nil {
		return file, "", "", e
	}
	return filepath.Clean(file), digest, release.Tag, nil
}
