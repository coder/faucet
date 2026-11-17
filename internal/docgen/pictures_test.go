package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
)

const (
	pictureSource = "docs/diagrams/source.html"
	pictureHash   = "docs/images/SOURCE_SHA256"
	pictureDir    = "docs/images"
	refreshImages = "run `cd verify && npm ci && npm run docs:images` and commit docs/images"
)

var (
	pictureRE  = regexp.MustCompile(`data-picture="([^"]*)"`)
	pngMagic   = []byte("\x89PNG\r\n\x1a\n")
	colorModes = []string{"light", "dark"}
)

// checkPictures fails unless docs/images holds a light and a dark PNG of each
// picture of the source page, and no other PNG, and SOURCE_SHA256 is the hash
// of that page, so a changed page without new images is stale.
func checkPictures(fsys fs.FS) ([]string, error) {
	src, err := fs.ReadFile(fsys, pictureSource)
	if err != nil {
		return nil, err
	}
	var problems []string
	sum := sha256.Sum256(src)
	want := hex.EncodeToString(sum[:]) + "  " + pictureSource + "\n"
	if got, err := fs.ReadFile(fsys, pictureHash); err != nil || string(got) != want {
		problems = append(problems, fmt.Sprintf("%s does not match %s: %s", pictureHash, pictureSource, refreshImages))
	}
	expected := map[string]bool{}
	for _, m := range pictureRE.FindAllSubmatch(src, -1) {
		for _, mode := range colorModes {
			expected[fmt.Sprintf("%s-%s.png", m[1], mode)] = true
		}
	}
	if len(expected) == 0 {
		problems = append(problems, pictureSource+" has no data-picture element")
	}
	for name := range expected {
		png, err := fs.ReadFile(fsys, path.Join(pictureDir, name))
		if err != nil || !bytes.HasPrefix(png, pngMagic) {
			problems = append(problems, fmt.Sprintf("%s/%s is missing or not a PNG: %s", pictureDir, name, refreshImages))
		}
	}
	entries, err := fs.ReadDir(fsys, pictureDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".png") && !expected[e.Name()] {
			problems = append(problems, fmt.Sprintf("%s/%s is no picture of %s: delete it", pictureDir, e.Name(), pictureSource))
		}
	}
	return problems, nil
}

func TestPicturesMatchTheirSource(t *testing.T) {
	t.Parallel()
	problems, err := checkPictures(os.DirFS(repoRoot(t)))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

func TestCheckPictures(t *testing.T) {
	t.Parallel()
	page := `<figure data-picture="map"></figure>`
	sum := sha256.Sum256([]byte(page))
	hash := hex.EncodeToString(sum[:]) + "  " + pictureSource + "\n"
	png := string(pngMagic) + "rest"
	good := map[string]string{
		pictureSource:                 page,
		pictureHash:                   hash,
		pictureDir + "/map-light.png": png,
		pictureDir + "/map-dark.png":  png,
	}
	for _, tc := range []struct {
		name    string
		change  map[string]string
		without string
		want    string
	}{
		{name: "current"},
		{name: "a changed page", change: map[string]string{pictureSource: page + " "}, want: "does not match"},
		{name: "a missing dark image", without: pictureDir + "/map-dark.png", want: "map-dark.png is missing"},
		{name: "an image that is not a PNG", change: map[string]string{pictureDir + "/map-light.png": "GIF89a"}, want: "map-light.png is missing or not a PNG"},
		{name: "an image of no picture", change: map[string]string{pictureDir + "/old-light.png": png}, want: "old-light.png is no picture"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := map[string]string{}
			for name, content := range good {
				if name != tc.without {
					files[name] = content
				}
			}
			for name, content := range tc.change {
				files[name] = content
			}
			problems, err := checkPictures(mapFS(files))
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(problems, "\n")
			if tc.want == "" && got != "" {
				t.Fatalf("want no problem, got:\n%s", got)
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("want a problem with %q, got:\n%s", tc.want, got)
			}
		})
	}
}
