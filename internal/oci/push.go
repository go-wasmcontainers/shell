package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/go-wasmcontainers/engine/image"
)

// PushOptions says where and as whom to push.
type PushOptions struct {
	// Base is the registry's scheme://host ("https://ghcr.io").
	Base string
	// Repo is the repository path ("go-wasmcontainers/shell").
	Repo string
	Tags []string
	// Username and Password are sent, with Basic authentication, to the
	// token endpoint only (never to the registry itself, never logged).
	Username, Password string
	Client             *http.Client
	UserAgent          string
}

type pusher struct {
	opt    PushOptions
	client *http.Client
	mu     sync.Mutex
	token  string
}

// maxBody bounds what is read of any registry or token answer.
const maxBody = 1 << 20

// Push uploads the blobs (skipping those the registry has), then the
// manifest under each tag. It returns the manifest digest.
func Push(ctx context.Context, opt PushOptions, img *Image) (image.Digest, error) {
	base, err := url.Parse(opt.Base)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.Path != "" {
		return "", fmt.Errorf("registry base %q must be scheme://host", opt.Base)
	}
	if _, err := image.NormalizeName(opt.Repo); err != nil || strings.Contains(opt.Repo, ":") {
		return "", fmt.Errorf("repository %q is not a valid name", opt.Repo)
	}
	if len(opt.Tags) == 0 {
		return "", errors.New("no tag to push")
	}
	c := &http.Client{}
	if opt.Client != nil {
		cc := *opt.Client
		c = &cc
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
		}
		return nil
	}
	if opt.UserAgent == "" {
		opt.UserAgent = "go-wasmcontainers-shell-mkimage"
	}
	p := &pusher{opt: opt, client: c}
	digests := make([]string, 0, len(img.Blobs))
	for d := range img.Blobs {
		digests = append(digests, string(d))
	}
	sort.Strings(digests)
	for _, d := range digests {
		if err := p.blob(ctx, image.Digest(d), img.Blobs[image.Digest(d)]); err != nil {
			return "", fmt.Errorf("blob %s: %w", d, err)
		}
	}
	for _, tag := range opt.Tags {
		if !validTag(tag) {
			return "", fmt.Errorf("tag %q is not valid", tag)
		}
		resp, err := p.do(ctx, http.MethodPut, p.url("manifests", tag), image.MediaTypeOCIManifest, img.Manifest)
		if err != nil {
			return "", err
		}
		if err := expect(resp, http.StatusCreated); err != nil {
			return "", fmt.Errorf("manifest %s: %w", tag, err)
		}
	}
	return img.Desc.Digest, nil
}

// validTag follows the distribution spec's tag grammar
// [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}.
func validTag(t string) bool {
	if t == "" || len(t) > 128 {
		return false
	}
	for i, r := range t {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if i > 0 {
			ok = ok || r == '.' || r == '-'
		}
		if !ok {
			return false
		}
	}
	return true
}

func (p *pusher) url(kind, obj string) string {
	return strings.TrimSuffix(p.opt.Base, "/") + "/v2/" + p.opt.Repo + "/" + kind + "/" + obj
}

func expect(resp *http.Response, codes ...int) error {
	defer resp.Body.Close()
	for _, c := range codes {
		if resp.StatusCode == c {
			io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
			return nil
		}
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return fmt.Errorf("registry answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
}

func (p *pusher) blob(ctx context.Context, d image.Digest, b []byte) error {
	resp, err := p.do(ctx, http.MethodHead, p.url("blobs", string(d)), "", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil // end-2: already there
	}
	// end-4a: open an upload session.
	resp, err = p.do(ctx, http.MethodPost, p.url("blobs", "uploads/"), "", nil)
	if err != nil {
		return err
	}
	loc := resp.Header.Get("Location")
	reqURL := resp.Request.URL
	if err := expect(resp, http.StatusAccepted); err != nil {
		return err
	}
	if loc == "" {
		return errors.New("upload session without a Location")
	}
	u, err := reqURL.Parse(loc)
	if err != nil {
		return fmt.Errorf("upload Location %q: %w", loc, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("upload Location %q is not http(s)", loc)
	}
	q := u.Query()
	q.Set("digest", string(d))
	u.RawQuery = q.Encode()
	// end-6: monolithic PUT.
	resp, err = p.do(ctx, http.MethodPut, u.String(), "application/octet-stream", b)
	if err != nil {
		return err
	}
	return expect(resp, http.StatusCreated)
}

// do sends one request, answering one Bearer challenge.
func (p *pusher) do(ctx context.Context, method, u, ctype string, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, r)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", p.opt.UserAgent)
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		if body != nil {
			req.ContentLength = int64(len(body))
		}
		p.mu.Lock()
		tok := p.token
		p.mu.Unlock()
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := p.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return resp, nil
		}
		ch := resp.Header.Get("Www-Authenticate")
		resp.Body.Close()
		if err := p.authenticate(ctx, ch); err != nil {
			return nil, err
		}
	}
}

func (p *pusher) authenticate(ctx context.Context, challenge string) error {
	scheme, params := image.ParseChallenge(challenge)
	if !strings.EqualFold(scheme, "Bearer") {
		return fmt.Errorf("registry requires %q authentication, which is not supported", scheme)
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Host == "" || (realm.Scheme != "https" && realm.Scheme != "http") {
		return fmt.Errorf("token realm %q is not an http(s) URL", params["realm"])
	}
	if realm.Scheme == "http" && strings.HasPrefix(p.opt.Base, "https://") {
		return fmt.Errorf("token realm %q downgrades to plain HTTP", params["realm"])
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	// Ask for push and pull on this repository whatever the challenge
	// scope says (a HEAD answers with a pull-only scope).
	q.Set("scope", "repository:"+p.opt.Repo+":pull,push")
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", p.opt.UserAgent)
	if p.opt.Username != "" || p.opt.Password != "" {
		req.SetBasicAuth(p.opt.Username, p.opt.Password)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry token: %s", resp.Status)
	}
	var tr struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&tr); err != nil {
		return fmt.Errorf("registry token: %w", err)
	}
	tok := tr.Token
	if tok == "" {
		tok = tr.AccessToken
	}
	if tok == "" {
		return errors.New("registry token response carries no token")
	}
	p.mu.Lock()
	p.token = tok
	p.mu.Unlock()
	return nil
}
