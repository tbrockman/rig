package incus

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Image is the subset of an Incus image entry rig uses.
type Image struct {
	Fingerprint string            `json:"fingerprint"`
	Filename    string            `json:"filename"`
	Size        int64             `json:"size"`
	Type        string            `json:"type"`
	Public      bool              `json:"public"`
	AutoUpdate  bool              `json:"auto_update"`
	Properties  map[string]string `json:"properties"`
	Profiles    []string          `json:"profiles"`
	Aliases     []ImageAliasEntry `json:"aliases"`
	UploadedAt  time.Time         `json:"uploaded_at"`
}

type ImageAliasEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Short is the 12-character prefix Incus itself displays.
func (i *Image) Short() string {
	if len(i.Fingerprint) > 12 {
		return i.Fingerprint[:12]
	}
	return i.Fingerprint
}

func (c *Client) Images() ([]Image, error) {
	var out []Image
	_, err := c.Get("/1.0/images?recursion=1", &out)
	sort.Slice(out, func(a, b int) bool { return out[a].UploadedAt.After(out[b].UploadedAt) })
	return out, err
}

func (c *Client) Image(fingerprint string) (*Image, error) {
	var img Image
	if _, err := c.Get("/1.0/images/"+url.PathEscape(fingerprint), &img); err != nil {
		return nil, err
	}
	return &img, nil
}

func (c *Client) DeleteImage(fingerprint string) error {
	return c.Delete("/1.0/images/" + url.PathEscape(fingerprint))
}

// --- aliases -------------------------------------------------------------

func (c *Client) ImageExists(alias string) bool {
	_, err := c.AliasTarget(alias)
	return err == nil
}

// AliasTarget resolves an alias to the fingerprint it points at.
func (c *Client) AliasTarget(alias string) (string, error) {
	var a struct {
		Target string `json:"target"`
	}
	if _, err := c.Get("/1.0/images/aliases/"+url.PathEscape(alias), &a); err != nil {
		return "", err
	}
	return a.Target, nil
}

// SetAlias points alias at fingerprint, creating it if it does not exist.
//
// Retargeting rather than delete-and-recreate is deliberate: the alias is how
// `rig new` finds the base image, and a window where it does not resolve is a
// window where creating a project VM fails.
func (c *Client) SetAlias(alias, fingerprint, description string) error {
	body := map[string]string{"target": fingerprint, "description": description}
	if _, err := c.AliasTarget(alias); err != nil {
		body["name"] = alias
		return c.Post("/1.0/images/aliases", body, nil)
	}
	return c.Put("/1.0/images/aliases/"+url.PathEscape(alias), body)
}

// SetImageProperties merges properties onto an existing image.
//
// Read-modify-write with the ETag, because an image's ETag is computed from its
// properties: a blind write would clobber whatever else is there.
func (c *Client) SetImageProperties(fingerprint string, props map[string]string) error {
	var img Image
	etag, err := c.Get("/1.0/images/"+url.PathEscape(fingerprint), &img)
	if err != nil {
		return err
	}
	merged := map[string]string{}
	for k, v := range img.Properties {
		merged[k] = v
	}
	for k, v := range props {
		merged[k] = v
	}
	return c.PutWithETag("/1.0/images/"+url.PathEscape(fingerprint), map[string]any{
		"auto_update": img.AutoUpdate,
		"properties":  merged,
		"public":      img.Public,
		"profiles":    img.Profiles,
	}, etag)
}

// --- import --------------------------------------------------------------

// SplitImageFingerprint computes the fingerprint Incus will assign a split
// image: sha256 over the metadata tarball followed by the disk.
//
// Knowing it in advance is what makes a rebuild cheap. Identical bytes produce
// an identical fingerprint, so a build that changed nothing can skip a
// multi-gigabyte upload — and can adopt an image that is already on the host,
// which is the difference between this working on a fresh daemon and only
// working on one rig itself populated.
func SplitImageFingerprint(metadataPath, rootfsPath string) (string, error) {
	h := sha256.New()
	for _, p := range []string{metadataPath, rootfsPath} {
		f, err := os.Open(p)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type ImportOpts struct {
	MetadataPath string
	RootfsPath   string
	Filename     string            // recorded on the image; cosmetic
	Properties   map[string]string // merged over the metadata tarball's own
}

// ImportImage uploads a split image (metadata tarball + disk) and returns the
// fingerprint Incus assigned it. It sets no alias: the caller retargets only
// after a successful import, so a failed build never leaves the host without a
// base image.
//
// The upload is a streamed multipart body — the disk is gigabytes, and the part
// names are load-bearing. Incus decides the image type from them: "rootfs.img"
// makes a virtual-machine image, "rootfs" makes a container one, and a
// container image will not boot as a VM.
func (c *Client) ImportImage(o ImportOpts) (string, error) {
	meta, err := os.Open(o.MetadataPath)
	if err != nil {
		return "", err
	}
	defer meta.Close()
	rootfs, err := os.Open(o.RootfsPath)
	if err != nil {
		return "", err
	}
	defer rootfs.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		copyPart := func(field, name string, src io.Reader) error {
			w, err := mw.CreateFormFile(field, name)
			if err != nil {
				return err
			}
			_, err = io.Copy(w, src)
			return err
		}
		err := copyPart("metadata", filepath.Base(o.MetadataPath), meta)
		if err == nil {
			err = copyPart("rootfs.img", filepath.Base(o.RootfsPath), rootfs)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()

	req, err := http.NewRequest(http.MethodPost, "http://incus/1.0/images", pr)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if o.Filename != "" {
		req.Header.Set("X-Incus-filename", o.Filename)
	}
	if len(o.Properties) > 0 {
		props := url.Values{}
		for k, v := range o.Properties {
			props.Set(k, v)
		}
		// Merged over the metadata tarball's properties, so os/release survive.
		req.Header.Set("X-Incus-properties", props.Encode())
	}

	env, _, err := c.do(req)
	if err != nil {
		return "", err
	}

	// Unpacking and hashing a multi-gigabyte disk onto a storage pool takes far
	// longer than any other operation rig issues.
	var res struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := c.WaitOperation(env.Operation, 60*time.Minute, &res); err != nil {
		return "", err
	}
	if res.Fingerprint == "" {
		return "", fmt.Errorf("image imported but Incus reported no fingerprint")
	}
	return res.Fingerprint, nil
}
