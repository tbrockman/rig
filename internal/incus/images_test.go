package incus

import (
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parseQuery(q string) (url.Values, error) { return url.ParseQuery(q) }

// Knowing the fingerprint before uploading is what lets a rebuild that changed
// nothing skip a multi-gigabyte import. It is sha256 over the metadata tarball
// followed by the disk, exactly, or every build looks new.
func TestSplitImageFingerprintIsSha256OfMetadataThenDisk(t *testing.T) {
	dir := t.TempDir()
	meta := filepath.Join(dir, "meta.tar.xz")
	disk := filepath.Join(dir, "disk.qcow2")
	os.WriteFile(meta, []byte("META"), 0o644)
	os.WriteFile(disk, []byte("DISK"), 0o644)

	h := sha256.Sum256([]byte("METADISK"))
	want := hex.EncodeToString(h[:])
	got, err := SplitImageFingerprint(meta, disk)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("fingerprint = %s, want %s", got, want)
	}
	if swapped, _ := SplitImageFingerprint(disk, meta); swapped == got {
		t.Error("order matters: metadata first, then the disk")
	}
}

// The multipart part names decide the image type: "rootfs.img" is a VM image,
// "rootfs" a container one, and a container image will not boot as a VM.
func TestImportImageSendsASplitMultipartAndReadsTheFingerprint(t *testing.T) {
	dir := t.TempDir()
	meta := filepath.Join(dir, "meta.tar.xz")
	disk := filepath.Join(dir, "nixos.qcow2")
	os.WriteFile(meta, []byte("META"), 0o644)
	os.WriteFile(disk, []byte("DISKDISK"), 0o644)

	f := newFake(t)
	parts := map[string]string{}
	var props url.Values
	f.handle("POST /1.0/images", func(w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("content type: %v", err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b := new(strings.Builder)
			buf := make([]byte, 1024)
			for {
				n, err := p.Read(buf)
				b.Write(buf[:n])
				if err != nil {
					break
				}
			}
			parts[p.FormName()] = b.String()
		}
		props, _ = url.ParseQuery(r.Header.Get("X-Incus-Properties"))
		writeAsync(w, f.operation("import", map[string]any{
			"metadata": map[string]string{"fingerprint": "deadbeef"},
		}), nil)
	})

	fp, err := f.client().ImportImage(ImportOpts{
		MetadataPath: meta, RootfsPath: disk, Filename: "nixos.qcow2",
		Properties: map[string]string{"rig.source": "/nix/store/x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fp != "deadbeef" {
		t.Errorf("fingerprint = %q; it lives in the operation's metadata and nowhere else", fp)
	}
	if parts["metadata"] != "META" || parts["rootfs.img"] != "DISKDISK" {
		t.Errorf("parts = %v; want metadata and rootfs.img, in that order and by those names", parts)
	}
	if props.Get("rig.source") != "/nix/store/x" {
		t.Errorf("properties header = %v", props)
	}
	req, _ := f.find("POST", "/1.0/images")
	if req.Header.Get("X-Incus-Filename") != "nixos.qcow2" {
		t.Error("the filename header was not sent")
	}
}

// The alias is how `rig new` finds the image. Retargeting rather than
// delete-and-recreate means there is never a moment it does not resolve.
func TestSetAliasCreatesWhenMissingAndRetargetsWhenPresent(t *testing.T) {
	f := newFake(t)
	exists := false
	f.handle("GET /1.0/images/aliases/{name}", func(w http.ResponseWriter, r *http.Request) {
		if !exists {
			writeError(w, 404, "not found")
			return
		}
		writeSync(w, map[string]string{"target": "old"})
	})
	f.handle("POST /1.0/images/aliases", func(w http.ResponseWriter, r *http.Request) { writeSync(w, nil) })
	f.handle("PUT /1.0/images/aliases/{name}", func(w http.ResponseWriter, r *http.Request) { writeSync(w, nil) })
	c := f.client()

	if err := c.SetAlias("base", "fp1", "rig base image"); err != nil {
		t.Fatal(err)
	}
	post, ok := f.find("POST", "/1.0/images/aliases")
	if !ok {
		t.Fatal("a missing alias must be created with POST")
	}
	var body map[string]string
	decode(t, post.Body, &body)
	if body["name"] != "base" || body["target"] != "fp1" {
		t.Errorf("create body = %v", body)
	}

	exists = true
	if err := c.SetAlias("base", "fp2", "rig base image"); err != nil {
		t.Fatal(err)
	}
	put, ok := f.find("PUT", "/1.0/images/aliases/base")
	if !ok {
		t.Fatal("an existing alias must be retargeted with PUT, never deleted")
	}
	decode(t, put.Body, &body)
	if body["target"] != "fp2" {
		t.Errorf("retarget body = %v", body)
	}
}

// An image's ETag covers its properties, so stamping one means reading the
// others first or losing them.
func TestSetImagePropertiesMergesRatherThanReplaces(t *testing.T) {
	f := newFake(t)
	f.handle("GET /1.0/images/{fp}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"i1"`)
		writeSync(w, map[string]any{"fingerprint": "fp", "public": false,
			"properties": map[string]string{"os": "nixos", "release": "26.05"}})
	})
	f.handle("PUT /1.0/images/{fp}", func(w http.ResponseWriter, r *http.Request) { writeSync(w, nil) })

	if err := f.client().SetImageProperties("fp", map[string]string{"rig.built": "now"}); err != nil {
		t.Fatal(err)
	}
	put, _ := f.find("PUT", "/1.0/images/fp")
	if put.Header.Get("If-Match") != `"i1"` {
		t.Error("the image write must carry the ETag it read")
	}
	var body struct {
		Properties map[string]string `json:"properties"`
	}
	decode(t, put.Body, &body)
	if body.Properties["os"] != "nixos" || body.Properties["rig.built"] != "now" {
		t.Errorf("properties = %v; the tarball's own must survive the stamp", body.Properties)
	}
}
