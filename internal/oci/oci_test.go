package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/image"
	"github.com/go-wasmcontainers/engine/rootfs"
)

var epoch = time.Unix(0, 0).UTC()

func sample() []Entry {
	return []Entry{
		{Path: "bin/tool.wasm", Mode: 0o755, Data: []byte("\x00asm\x01\x00\x00\x00tool")},
		{Path: "bin/tool", Link: "tool.wasm"},
		{Path: "etc/passwd", Data: []byte("root:x:0:0::/root:/bin/sh\n")},
		{Path: "tmp", Dir: true, Mode: 0o1777},
	}
}

func TestLayerDeterministicAndAcceptedByTheEngine(t *testing.T) {
	a, da, err := Layer(sample(), epoch)
	if err != nil {
		t.Fatal(err)
	}
	rev := sample()
	rev[0], rev[3] = rev[3], rev[0]
	b, db, err := Layer(rev, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) || da != db {
		t.Fatal("layer depends on entry order")
	}
	f := rootfs.New()
	if err := f.ApplyLayer(bytes.NewReader(a), image.MediaTypeOCILayerGz, "", rootfs.NewBudget(rootfs.DefaultLimits)); err != nil {
		t.Fatalf("engine refused the layer: %v", err)
	}
	if l, err := f.Readlink("/bin/tool"); err != nil || l != "tool.wasm" {
		t.Errorf("link: %q %v", l, err)
	}
	if b, err := f.ReadFile("/bin/tool"); err != nil || !bytes.HasPrefix(b, []byte("\x00asm")) {
		t.Errorf("through the link: %q %v", b, err)
	}
	if _, err := f.Stat("/etc"); err != nil {
		t.Errorf("implicit parent: %v", err)
	}
}

func TestLayerRefusesBadPaths(t *testing.T) {
	for _, es := range [][]Entry{
		{{Path: "/abs"}},
		{{Path: "a/../../x"}},
		{{Path: ""}},
		{{Path: "a//b"}},
		{{Path: "x"}, {Path: "x"}},
		{{Path: "f"}, {Path: "f/g"}},
		make([]Entry, MaxLayerEntries+1),
	} {
		if _, _, err := Layer(es, epoch); err == nil {
			t.Errorf("accepted %v", es[:min(len(es), 2)])
		}
	}
}

func build(t *testing.T) *Image {
	t.Helper()
	gz, diff, err := Layer(sample(), epoch)
	if err != nil {
		t.Fatal(err)
	}
	img, err := Build(Spec{Layer: gz, DiffID: diff, Created: epoch,
		Exec:        image.ExecConfig{Cmd: []string{"/bin/sh"}, Shell: []string{"/bin/sh", "-c"}},
		Annotations: map[string]string{"org.opencontainers.image.source": "https://example.org/x"}})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestLayoutPullsIntoTheEngine(t *testing.T) {
	img := build(t)
	dir := t.TempDir()
	if err := WriteLayout(dir, img, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	src, _, err := image.OpenSource("oci:"+dir+":0.1.0", image.RegistryOptions{}, image.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	st, err := image.NewStore(image.NewMemoryBackend(1 << 30))
	if err != nil {
		t.Fatal(err)
	}
	res, err := image.Pull(context.Background(), st, src, image.DefaultLimits, nil)
	if err != nil {
		t.Fatalf("engine cannot pull the layout: %v", err)
	}
	c := res.Image.Config
	if c.OS != "wasip1" || c.Architecture != "wasm" || strings.Join(c.Config.Shell, " ") != "/bin/sh -c" {
		t.Errorf("config: %+v", c)
	}
	if _, err := Build(Spec{}); err == nil {
		t.Error("empty spec built")
	}
	bad := &Image{Blobs: map[image.Digest][]byte{"sha256:00": []byte("x")}, Manifest: img.Manifest, Desc: img.Desc}
	if err := WriteLayout(t.TempDir(), bad, ""); err == nil {
		t.Error("bad digest written")
	}
	wrong := &Image{Blobs: map[image.Digest][]byte{image.FromBytes([]byte("y")): []byte("x")}, Manifest: img.Manifest, Desc: img.Desc}
	if err := WriteLayout(t.TempDir(), wrong, ""); err == nil {
		t.Error("mismatched blob written")
	}
}

// registry is an in-memory OCI distribution registry behind a Bearer
// token service, enough for end-2/4a/6/7a.
type registry struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string][]byte
	uploads   int
	user, pw  string
	srv       *httptest.Server
	tokenAsks int
}

func newRegistry(t *testing.T) *registry {
	r := &registry{blobs: map[string][]byte{}, manifests: map[string][]byte{}, user: "u", pw: "secret"}
	r.srv = httptest.NewServer(r)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *registry) ServeHTTP(w http.ResponseWriter, q *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if q.URL.Path == "/token" {
		r.tokenAsks++
		u, p, ok := q.BasicAuth()
		if !ok || u != r.user || p != r.pw || q.URL.Query().Get("scope") != "repository:org/img:pull,push" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"token": "T"})
		return
	}
	if q.Header.Get("Authorization") != "Bearer T" {
		w.Header().Set("Www-Authenticate", `Bearer realm="`+r.srv.URL+`/token",service="test",scope="repository:org/img:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	const pre = "/v2/org/img/"
	p, ok := strings.CutPrefix(q.URL.Path, pre)
	if !ok {
		http.NotFound(w, q)
		return
	}
	body, _ := io.ReadAll(q.Body)
	switch {
	case q.Method == http.MethodHead && strings.HasPrefix(p, "blobs/"):
		if _, ok := r.blobs[strings.TrimPrefix(p, "blobs/")]; !ok {
			w.WriteHeader(http.StatusNotFound)
		}
	case q.Method == http.MethodPost && p == "blobs/uploads/":
		r.uploads++
		w.Header().Set("Location", "/v2/org/img/blobs/uploads/session?state=x")
		w.WriteHeader(http.StatusAccepted)
	case q.Method == http.MethodPut && p == "blobs/uploads/session":
		d := q.URL.Query().Get("digest")
		if q.URL.Query().Get("state") != "x" || string(image.FromBytes(body)) != d {
			http.Error(w, "digest", http.StatusBadRequest)
			return
		}
		r.blobs[d] = body
		w.WriteHeader(http.StatusCreated)
	case q.Method == http.MethodPut && strings.HasPrefix(p, "manifests/"):
		if q.Header.Get("Content-Type") != image.MediaTypeOCIManifest {
			http.Error(w, "type", http.StatusBadRequest)
			return
		}
		var m image.Manifest
		json.Unmarshal(body, &m)
		for _, d := range append([]image.Descriptor{m.Config}, m.Layers...) {
			if _, ok := r.blobs[string(d.Digest)]; !ok {
				http.Error(w, "blob unknown", http.StatusBadRequest)
				return
			}
		}
		r.manifests[strings.TrimPrefix(p, "manifests/")] = body
		w.WriteHeader(http.StatusCreated)
	default:
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
	}
}

func TestPush(t *testing.T) {
	img := build(t)
	reg := newRegistry(t)
	opt := PushOptions{Base: reg.srv.URL, Repo: "org/img", Tags: []string{"0.1.0", "latest"}, Username: "u", Password: "secret"}
	d, err := Push(context.Background(), opt, img)
	if err != nil {
		t.Fatal(err)
	}
	if d != img.Desc.Digest || !bytes.Equal(reg.manifests["latest"], img.Manifest) || len(reg.blobs) != 2 {
		t.Fatalf("registry holds %d blobs, manifests %v", len(reg.blobs), len(reg.manifests))
	}
	// A second push uploads nothing: every blob is already there.
	before := reg.uploads
	if _, err := Push(context.Background(), opt, img); err != nil {
		t.Fatal(err)
	}
	if reg.uploads != before {
		t.Errorf("re-uploaded %d blobs", reg.uploads-before)
	}
}

func TestPushRefusals(t *testing.T) {
	img := build(t)
	reg := newRegistry(t)
	for name, opt := range map[string]PushOptions{
		"wrong password": {Base: reg.srv.URL, Repo: "org/img", Tags: []string{"t"}, Username: "u", Password: "nope"},
		"no tag":         {Base: reg.srv.URL, Repo: "org/img", Username: "u", Password: "secret"},
		"bad tag":        {Base: reg.srv.URL, Repo: "org/img", Tags: []string{"-x"}, Username: "u", Password: "secret"},
		"bad base":       {Base: "ftp://x", Repo: "org/img", Tags: []string{"t"}},
		"base with path": {Base: reg.srv.URL + "/p", Repo: "org/img", Tags: []string{"t"}},
		"bad repo":       {Base: reg.srv.URL, Repo: "Org/IMG:x", Tags: []string{"t"}},
		"other repo":     {Base: reg.srv.URL, Repo: "org/other", Tags: []string{"t"}, Username: "u", Password: "secret"},
	} {
		if _, err := Push(context.Background(), opt, img); err == nil {
			t.Errorf("%s: pushed", name)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: the password leaks into the error: %v", name, err)
		}
	}
	if !validTag("v0.1.0") || !validTag("latest") || validTag("") || validTag(strings.Repeat("a", 129)) || validTag("a/b") {
		t.Error("validTag")
	}
}

func TestChallengeErrors(t *testing.T) {
	img := build(t)
	cases := map[string]string{
		"basic":     `Basic realm="x"`,
		"no realm":  `Bearer service="x"`,
		"downgrade": `Bearer realm="http://example.invalid/token"`,
	}
	for name, ch := range cases {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
			w.Header().Set("Www-Authenticate", ch)
			w.WriteHeader(http.StatusUnauthorized)
		}))
		_, err := Push(context.Background(), PushOptions{Base: srv.URL, Repo: "org/img", Tags: []string{"t"}, Client: srv.Client()}, img)
		srv.Close()
		if err == nil {
			t.Errorf("%s: pushed", name)
		}
	}
	// A token service that answers without a token.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/token" {
			w.Write([]byte(`{}`))
			return
		}
		w.Header().Set("Www-Authenticate", `Bearer realm="`+srv.URL+`/token"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := Push(context.Background(), PushOptions{Base: srv.URL, Repo: "org/img", Tags: []string{"t"}}, img); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Errorf("empty token: %v", err)
	}
}
