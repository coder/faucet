package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	pictureSource = "docs/diagrams/source.html"
	pictureHash   = "docs/images/SOURCE_SHA256"
	pictureDir    = "docs/images"
	refreshImages = "run `cd verify && npm ci && npx playwright-core install chromium && npm run docs:images` and commit docs/images"
	readme        = "README.md"
)

var (
	pictureRE = regexp.MustCompile(`data-picture="([^"]*)"`)
	// hashLineRE is one line that sha256sum writes in text mode.
	hashLineRE = regexp.MustCompile(`^([0-9a-f]{64})  (\S+)\n$`)
	colorModes = []string{"light", "dark"}

	pictureBlockRE = regexp.MustCompile(`(?s)<picture>(.*?)</picture>`)
	sourceRE       = regexp.MustCompile(`<source media="\(prefers-color-scheme: (dark|light)\)" srcset="([^"]*)">`)
	imgRE          = regexp.MustCompile(`<img [^>]*>`)
	altRE          = regexp.MustCompile(` alt="([^"]*)"`)
	srcRE          = regexp.MustCompile(` src="([^"]*)"`)
)

// checkPictures fails unless SOURCE_SHA256 lists, each with its current hash,
// exactly the source page and the light and dark PNG of each of its pictures,
// and docs/images holds no other PNG. A page or an image that changed without
// a new render is stale.
func checkPictures(fsys fs.FS) ([]string, error) {
	src, err := fs.ReadFile(fsys, pictureSource)
	if err != nil {
		return nil, err
	}
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	want := map[string]bool{pictureSource: true}
	var names []string
	for _, m := range pictureRE.FindAllSubmatch(src, -1) {
		names = append(names, string(m[1]))
		for _, mode := range colorModes {
			want[fmt.Sprintf("%s/%s-%s.png", pictureDir, m[1], mode)] = true
		}
	}
	if len(want) == 1 {
		report("%s has no data-picture element", pictureSource)
	}
	readmeText, err := fs.ReadFile(fsys, readme)
	if err != nil {
		return nil, err
	}
	problems = append(problems, checkPictureMarkup(string(readmeText), names)...)

	sums, err := fs.ReadFile(fsys, pictureHash)
	if err != nil {
		report("%s is missing: %s", pictureHash, refreshImages)
	}
	lines := strings.SplitAfter(string(sums), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	listed := map[string]bool{}
	for i, line := range lines {
		m := hashLineRE.FindStringSubmatch(line)
		if m == nil {
			report("%s line %d is not `<sha256>  <path>` and a newline: %s", pictureHash, i+1, refreshImages)
			continue
		}
		name := m[2]
		listed[name] = true
		if !want[name] {
			report("%s lists %s, which is neither %s nor an image of one of its pictures: %s", pictureHash, name, pictureSource, refreshImages)
			continue
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			report("%s is missing: %s", name, refreshImages)
			continue
		}
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != m[1] {
			report("%s does not match its hash in %s: %s", name, pictureHash, refreshImages)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if !listed[name] {
			report("%s has no line in %s: %s", name, pictureHash, refreshImages)
		}
	}

	entries, err := fs.ReadDir(fsys, pictureDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := path.Join(pictureDir, e.Name())
		if strings.HasSuffix(name, ".png") && !want[name] && !listed[name] {
			report("%s is not listed in %s and is no picture of %s: delete it", name, pictureHash, pictureSource)
		}
	}
	return problems, nil
}

// checkPictureMarkup fails unless the README shows each picture once, in a
// <picture> whose dark source is its dark PNG and whose light source and
// fallback <img> are its light PNG, with alt text, and shows no other.
func checkPictureMarkup(text string, names []string) []string {
	var problems []string
	shown := map[string]int{}
	for i, block := range pictureBlockRE.FindAllStringSubmatch(text, -1) {
		srcsets := map[string]string{}
		for _, m := range sourceRE.FindAllStringSubmatch(block[1], -1) {
			srcsets[m[1]] = m[2]
		}
		img := imgRE.FindString(block[1])
		var alt, fallback string
		if m := altRE.FindStringSubmatch(img); m != nil {
			alt = m[1]
		}
		if m := srcRE.FindStringSubmatch(img); m != nil {
			fallback = m[1]
		}
		name := strings.TrimSuffix(strings.TrimPrefix(srcsets["light"], pictureDir+"/"), "-light.png")
		if !slices.Contains(names, name) {
			problems = append(problems, fmt.Sprintf("%s <picture> %d shows %q, which is no picture of %s", readme, i+1, srcsets["light"], pictureSource))
			continue
		}
		shown[name]++
		image := func(mode string) string { return fmt.Sprintf("%s/%s-%s.png", pictureDir, name, mode) }
		if srcsets["dark"] != image("dark") || fallback != image("light") || strings.TrimSpace(alt) == "" {
			problems = append(problems, fmt.Sprintf("%s <picture> %d of %s must have a dark source %s, a light source and an <img src> %s, and alt text", readme, i+1, name, image("dark"), image("light")))
		}
	}
	for _, name := range names {
		if shown[name] != 1 {
			problems = append(problems, fmt.Sprintf("%s shows the picture %s %d times, not once", readme, name, shown[name]))
		}
	}
	return problems
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
	const (
		page     = `<figure data-picture="map"></figure>`
		light    = "\x89PNG\r\n\x1a\nlight"
		dark     = "\x89PNG\r\n\x1a\ndark"
		lightPNG = pictureDir + "/map-light.png"
		darkPNG  = pictureDir + "/map-dark.png"
	)
	line := func(name, content string) string {
		sum := sha256.Sum256([]byte(content))
		return hex.EncodeToString(sum[:]) + "  " + name + "\n"
	}
	sums := line(pictureSource, page) + line(darkPNG, dark) + line(lightPNG, light)
	markup := func(dark, light, img, alt string) string {
		return "<picture>\n" +
			`  <source media="(prefers-color-scheme: dark)" srcset="` + dark + `">` + "\n" +
			`  <source media="(prefers-color-scheme: light)" srcset="` + light + `">` + "\n" +
			`  <img alt="` + alt + `" src="` + img + `">` + "\n</picture>\n"
	}
	shown := markup(darkPNG, lightPNG, lightPNG, "a map")
	good := map[string]string{
		readme:        shown,
		pictureSource: page,
		pictureHash:   sums,
		lightPNG:      light,
		darkPNG:       dark,
	}
	for _, tc := range []struct {
		name    string
		change  map[string]string
		without string
		want    string
	}{
		{name: "current"},
		{name: "a changed page", change: map[string]string{pictureSource: page + " "}, want: pictureSource + " does not match its hash"},
		{name: "swapped images", change: map[string]string{lightPNG: dark, darkPNG: light}, want: lightPNG + " does not match its hash"},
		{name: "a truncated image", change: map[string]string{darkPNG: dark[:8]}, want: darkPNG + " does not match its hash"},
		{name: "a missing image", without: darkPNG, want: darkPNG + " is missing"},
		{name: "no hash file", without: pictureHash, want: pictureHash + " is missing"},
		{name: "a malformed line", change: map[string]string{pictureHash: sums + "not a hash\n"}, want: "line 4 is not"},
		{name: "no newline after the last line", change: map[string]string{pictureHash: strings.TrimSuffix(sums, "\n")}, want: "line 3 is not"},
		{name: "a line for no picture", change: map[string]string{pictureHash: sums + line("README.md", "x"), "README.md": "x"}, want: "lists README.md, which is neither"},
		{name: "an image without a line", change: map[string]string{pictureHash: line(pictureSource, page) + line(lightPNG, light)}, want: darkPNG + " has no line"},
		{name: "a page without a line", change: map[string]string{pictureHash: line(darkPNG, dark) + line(lightPNG, light)}, want: pictureSource + " has no line"},
		{name: "an unlisted image of no picture", change: map[string]string{pictureDir + "/old-light.png": light}, want: pictureDir + "/old-light.png is not listed"},
		{name: "a page without pictures", change: map[string]string{pictureSource: "<p></p>", pictureHash: line(pictureSource, "<p></p>")}, want: "has no data-picture element"},
		{name: "a README with the dark and light sources swapped", change: map[string]string{readme: markup(lightPNG, darkPNG, lightPNG, "a map")}, want: "which is no picture"},
		{name: "a README whose dark source is the light image", change: map[string]string{readme: markup(lightPNG, lightPNG, lightPNG, "a map")}, want: "must have a dark source"},
		{name: "a README whose fallback is the dark image", change: map[string]string{readme: markup(darkPNG, lightPNG, darkPNG, "a map")}, want: "must have a dark source"},
		{name: "a README picture without alt text", change: map[string]string{readme: markup(darkPNG, lightPNG, lightPNG, " ")}, want: "and alt text"},
		{name: "a README without the picture", change: map[string]string{readme: "no pictures"}, want: "shows the picture map 0 times"},
		{name: "a README with the picture twice", change: map[string]string{readme: shown + shown}, want: "shows the picture map 2 times"},
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
