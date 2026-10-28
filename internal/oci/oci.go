// Package oci builds a single-layer OCI image for wasip1/wasm, writes it
// as an OCI image layout and pushes it to a registry.
//
// References (docs/REFERENCES.md): OCI image-spec image-layout.md
// (oci-layout, index.json, blobs/<alg>/<encoded>) and layer.md (tar
// changesets: mode, mtime, symlink linkname); OCI distribution-spec
// (end-2, end-4a/end-6, end-7a); the registry token flow
// (distribution's auth/token: Bearer realm/service/scope, token or
// access_token). Types and digests are the engine's (engine/image).
package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-wasmcontainers/engine/image"
)

// Entry is one path of the layer.
type Entry struct {
	// Path is relative to the root ("bin/sh"); directories end without a
	// slash and set Dir.
	Path string
	Dir  bool
	// Link, if set, makes the entry a symbolic link to Link.
	Link string
	Mode int64
	Data []byte
}

// MaxLayerEntries bounds what Layer accepts.
const MaxLayerEntries = 4096

// cleanPath validates a layer path: relative, no "..", no empty element.
func cleanPath(p string) (string, error) {
	c := path.Clean("/" + p)[1:]
	if c == "" || c != strings.TrimSuffix(p, "/") || strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("layer path %q is not a clean relative path", p)
	}
	for _, el := range strings.Split(c, "/") {
		if el == ".." {
			return "", fmt.Errorf("layer path %q escapes the root", p)
		}
	}
	return c, nil
}

// Layer builds a deterministic tar+gzip layer: entries sorted by path,
// parents before children, every timestamp mtime, owner 0:0. It returns
// the compressed layer and the digest of the uncompressed tar (the
// config's diff_id).
func Layer(entries []Entry, mtime time.Time) (gz []byte, diffID image.Digest, err error) {
	if len(entries) > MaxLayerEntries {
		return nil, "", fmt.Errorf("layer has %d entries, more than %d", len(entries), MaxLayerEntries)
	}
	byPath := map[string]Entry{}
	for _, e := range entries {
		c, err := cleanPath(e.Path)
		if err != nil {
			return nil, "", err
		}
		if _, dup := byPath[c]; dup {
			return nil, "", fmt.Errorf("layer path %q given twice", c)
		}
		e.Path = c
		byPath[c] = e
	}
	// Implicit parent directories.
	for c := range byPath {
		for d := path.Dir(c); d != "."; d = path.Dir(d) {
			if p, ok := byPath[d]; ok {
				if !p.Dir {
					return nil, "", fmt.Errorf("layer path %q is under %q, which is not a directory", c, d)
				}
				continue
			}
			byPath[d] = Entry{Path: d, Dir: true, Mode: 0o755}
		}
	}
	names := make([]string, 0, len(byPath))
	for n := range byPath {
		names = append(names, n)
	}
	sort.Strings(names)
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, n := range names {
		e := byPath[n]
		h := &tar.Header{Name: n, Mode: e.Mode, ModTime: mtime, Format: tar.FormatPAX}
		switch {
		case e.Dir:
			h.Typeflag, h.Name = tar.TypeDir, n+"/"
		case e.Link != "":
			h.Typeflag, h.Linkname = tar.TypeSymlink, e.Link
		default:
			h.Typeflag, h.Size = tar.TypeReg, int64(len(e.Data))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
			if e.Dir || e.Link != "" {
				h.Mode = 0o755
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			return nil, "", err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(e.Data); err != nil {
				return nil, "", err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	var out bytes.Buffer
	zw, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		return nil, "", err
	}
	if _, err := zw.Write(raw.Bytes()); err != nil {
		return nil, "", err
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	return out.Bytes(), image.FromBytes(raw.Bytes()), nil
}

// Image is a built image: its blobs by digest, the manifest, and the
// descriptor of the manifest.
type Image struct {
	Blobs    map[image.Digest][]byte
	Manifest []byte
	Desc     image.Descriptor
}

// Spec describes the image to build.
type Spec struct {
	Layer   []byte
	DiffID  image.Digest
	Exec    image.ExecConfig
	Created time.Time
	// Annotations go on the manifest (e.g. org.opencontainers.image.source,
	// which links a ghcr.io package to its repository).
	Annotations map[string]string
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Build assembles config and manifest around one tar+gzip layer, for the
// platform wasip1/wasm.
func Build(s Spec) (*Image, error) {
	if len(s.Layer) == 0 || s.DiffID == "" {
		return nil, errors.New("image needs a layer and its diff_id")
	}
	cfg := image.Config{
		Created: s.Created.UTC().Format(time.RFC3339), Architecture: image.WasmPlatform.Architecture, OS: image.WasmPlatform.OS,
		Config: s.Exec,
		RootFS: image.RootFS{Type: "layers", DiffIDs: []string{string(s.DiffID)}},
	}
	cb := mustJSON(cfg)
	img := &Image{Blobs: map[image.Digest][]byte{}}
	img.Blobs[image.FromBytes(cb)] = cb
	img.Blobs[image.FromBytes(s.Layer)] = s.Layer
	m := image.Manifest{
		SchemaVersion: 2,
		MediaType:     image.MediaTypeOCIManifest,
		Config:        image.Descriptor{MediaType: image.MediaTypeOCIConfig, Digest: image.FromBytes(cb), Size: int64(len(cb))},
		Layers:        []image.Descriptor{{MediaType: image.MediaTypeOCILayerGz, Digest: image.FromBytes(s.Layer), Size: int64(len(s.Layer))}},
		Annotations:   s.Annotations,
	}
	img.Manifest = mustJSON(m)
	img.Desc = image.Descriptor{MediaType: image.MediaTypeOCIManifest, Digest: image.FromBytes(img.Manifest),
		Size: int64(len(img.Manifest)), Platform: &image.WasmPlatform}
	return img, nil
}

// WriteLayout writes img as an OCI image layout in dir, the index entry
// annotated with refName (org.opencontainers.image.ref.name).
func WriteLayout(dir string, img *Image, refName string) error {
	blobDir := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		return err
	}
	put := func(d image.Digest, b []byte) error {
		alg, hexd, ok := strings.Cut(string(d), ":")
		if !ok || alg != "sha256" || len(hexd) != 64 {
			return fmt.Errorf("bad digest %q", d)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != hexd {
			return fmt.Errorf("blob does not match its digest %s", d)
		}
		return os.WriteFile(filepath.Join(blobDir, hexd), b, 0o644)
	}
	for d, b := range img.Blobs {
		if err := put(d, b); err != nil {
			return err
		}
	}
	if err := put(img.Desc.Digest, img.Manifest); err != nil {
		return err
	}
	desc := img.Desc
	if refName != "" {
		desc.Annotations = map[string]string{image.AnnotationRefName: refName}
	}
	idx := image.Index{SchemaVersion: 2, MediaType: image.MediaTypeOCIIndex, Manifests: []image.Descriptor{desc}}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), mustJSON(idx), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644)
}
