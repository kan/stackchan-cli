package main

import (
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePNG saves a w×h image whose pixel color is given by at.
func writePNG(t *testing.T, path string, w, h int, at func(x, y int) color.RGBA) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, at(x, y))
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func solid(c color.RGBA) func(x, y int) color.RGBA {
	return func(int, int) color.RGBA { return c }
}

// checkAllPixels fails unless every RGB565 (little-endian) pixel is want.
func checkAllPixels(t *testing.T, frame []byte, want uint16) {
	t.Helper()
	if len(frame) != avatarW*avatarH*2 {
		t.Fatalf("frame is %d bytes, want %d", len(frame), avatarW*avatarH*2)
	}
	for i := 0; i < len(frame); i += 2 {
		if got := binary.LittleEndian.Uint16(frame[i:]); got != want {
			t.Fatalf("pixel %d = %#04x, want %#04x", i/2, got, want)
		}
	}
}

func TestLoadAvatarFrameSolidColor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.png")
	// R 0xFF -> 0xF8<<8, G 0x80 -> 0x80<<3, B 0x40 -> 0x40>>3
	writePNG(t, path, 320, 240, solid(color.RGBA{0xFF, 0x80, 0x40, 0xFF}))
	frame, err := loadAvatarFrame(path)
	if err != nil {
		t.Fatal(err)
	}
	checkAllPixels(t, frame, 0xFC08)
}

func TestLoadAvatarFrameAveragesEachBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.png")
	// Black/white checkerboard: every 2×2 block averages to 127 (0x7F).
	writePNG(t, path, 320, 240, func(x, y int) color.RGBA {
		if (x+y)%2 == 0 {
			return color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
		}
		return color.RGBA{0, 0, 0, 0xFF}
	})
	frame, err := loadAvatarFrame(path)
	if err != nil {
		t.Fatal(err)
	}
	checkAllPixels(t, frame, 0x7BEF) // 0x78<<8 | 0x7C<<3 | 0x7F>>3
}

func TestLoadAvatarFrameNativeSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.png")
	writePNG(t, path, 160, 120, solid(color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}))
	frame, err := loadAvatarFrame(path)
	if err != nil {
		t.Fatal(err)
	}
	checkAllPixels(t, frame, 0xFFFF)
}

// A 0×0 PNG cannot be made: the encoder and decoder both reject it before
// loadAvatarFrame's own size check.
func TestLoadAvatarFrameRejectsBadSizes(t *testing.T) {
	dir := t.TempDir()
	for _, size := range [][2]int{{100, 100}, {161, 120}, {320, 120}, {160, 240}} {
		path := filepath.Join(dir, "f.png")
		writePNG(t, path, size[0], size[1], solid(color.RGBA{A: 0xFF}))
		if _, err := loadAvatarFrame(path); err == nil || !strings.Contains(err.Error(), "not an integer multiple") {
			t.Errorf("%dx%d: err = %v, want a size error", size[0], size[1], err)
		}
	}
	notPNG := filepath.Join(dir, "x.png")
	if err := os.WriteFile(notPNG, []byte("not a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAvatarFrame(notPNG); err == nil {
		t.Error("a non-PNG file was accepted")
	}
}

func TestBuildAvatarPayload(t *testing.T) {
	fakeHome(t)
	if err := os.MkdirAll(avatarDir(""), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range avatarFrames {
		writePNG(t, filepath.Join(avatarDir(""), name+".png"), 160, 120, solid(color.RGBA{A: 0xFF}))
	}
	if len(avatarFrames) != 14 {
		t.Fatalf("layered mode has 14 frames, avatarFrames has %d", len(avatarFrames))
	}

	path, err := buildAvatarPayload("")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 537600 { // 14 × 160 × 120 × 2
		t.Errorf("payload is %d bytes, want 537600", info.Size())
	}

	// Each call writes its own file, so concurrent uploads don't collide.
	path2, err := buildAvatarPayload("")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path2)
	if path2 == path {
		t.Errorf("two payloads share the file %s", path)
	}
}

func TestBuildAvatarPayloadMissingFrame(t *testing.T) {
	fakeHome(t)
	if err := os.MkdirAll(avatarDir(""), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range avatarFrames[1:] { // no idle.png
		writePNG(t, filepath.Join(avatarDir(""), name+".png"), 160, 120, solid(color.RGBA{A: 0xFF}))
	}
	if _, err := buildAvatarPayload(""); err == nil || !strings.Contains(err.Error(), "idle.png") {
		t.Errorf("err = %v, want an error naming idle.png", err)
	}
}

func TestBuildAvatarPayloadNamedSet(t *testing.T) {
	fakeHome(t)
	if got, want := avatarDir("sleep"), filepath.Join(stackchanDir(), "avatar-sleep"); got != want {
		t.Errorf("avatarDir(sleep) = %s, want %s", got, want)
	}
	if _, err := buildAvatarPayload("sleep"); err == nil || !strings.Contains(err.Error(), "gen_avatars.py --sleep") {
		t.Errorf("missing sleep set: err = %v, want a hint to make it", err)
	}
	// Default set black, sleep set white: the payload must come from the sleep set.
	for set, c := range map[string]color.RGBA{"": {A: 0xFF}, "sleep": {R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}} {
		if err := os.MkdirAll(avatarDir(set), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range avatarFrames {
			writePNG(t, filepath.Join(avatarDir(set), name+".png"), 160, 120, solid(c))
		}
	}
	path, err := buildAvatarPayload("sleep")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if data[0] != 0xFF || data[1] != 0xFF {
		t.Errorf("first pixel = %02x %02x, want white (ff ff) from the sleep set", data[0], data[1])
	}

	for _, bad := range []string{"../x", "a/b", `a\b`, "..", "sleep set"} {
		if _, err := buildAvatarPayload(bad); err == nil {
			t.Errorf("set name %q was accepted", bad)
		}
	}
}
