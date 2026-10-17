package main

// Custom avatar upload. Firmware >= 1.9 renders faces from an AvatarSet that
// the gateway sends at runtime (load_avatar_set); the prebuilt firmware only
// has black placeholders, and a loaded set lives in PSRAM, so it must be sent
// again after every device reboot. The daemon does that on each new session.
//
// Source images: ~/.stackchan/avatar/<name>.png (the same files the old
// build-time converter used), layered mode = 14 frames in this order.

import (
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"

	"stackchan-cli/internal/mcp"
)

var avatarFrames = []string{
	"idle", "happy", "thinking", "sad", "surprised", "embarrassed", // faces
	"eyes_open", "eyes_half", "eyes_closed",
	"mouth_closed", "mouth_half", "mouth_open", "mouth_e", "mouth_u",
}

const (
	avatarW           = 160
	avatarH           = 120
	avatarLoadTimeout = 120 // seconds; power-save Wi-Fi can make the fetch slow
)

func avatarDir() string { return filepath.Join(stackchanDir(), "avatar") }

// buildAvatarPayload converts the PNGs into one raw little-endian RGB565 file
// (14 × 160×120) and returns its path; the caller removes it after the upload.
// Each call gets its own file so concurrent uploads (daemon reconnect + a
// manual avatar-load) never overwrite a file the gateway is serving.
// Images whose size is an integer multiple of 160×120 (e.g. 320×240) are
// box-downscaled.
func buildAvatarPayload() (string, error) {
	out := make([]byte, 0, len(avatarFrames)*avatarW*avatarH*2)
	for _, name := range avatarFrames {
		frame, err := loadAvatarFrame(filepath.Join(avatarDir(), name+".png"))
		if err != nil {
			return "", err
		}
		out = append(out, frame...)
	}
	f, err := os.CreateTemp(stackchanDir(), "avatar_set-*.rgb565")
	if err != nil {
		return "", err
	}
	_, werr := f.Write(out)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func loadAvatarFrame(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	src, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	b := src.Bounds()
	if b.Dx() == 0 || b.Dx()%avatarW != 0 || b.Dy()%avatarH != 0 || b.Dx()/avatarW != b.Dy()/avatarH {
		return nil, fmt.Errorf("%s: size %dx%d is not an integer multiple of %dx%d", path, b.Dx(), b.Dy(), avatarW, avatarH)
	}
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)

	k := b.Dx() / avatarW
	out := make([]byte, 0, avatarW*avatarH*2)
	for y := range avatarH {
		for x := range avatarW {
			var r, g, bl int
			for dy := range k {
				for dx := range k {
					i := rgba.PixOffset(x*k+dx, y*k+dy)
					r += int(rgba.Pix[i])
					g += int(rgba.Pix[i+1])
					bl += int(rgba.Pix[i+2])
				}
			}
			n := k * k
			v := uint16(r/n&0xF8)<<8 | uint16(g/n&0xFC)<<3 | uint16(bl/n>>3)
			out = append(out, byte(v), byte(v>>8))
		}
	}
	return out, nil
}

// withAvatarPayload builds the payload, runs upload with the load_avatar_set
// arguments, and removes the payload file afterwards.
func withAvatarPayload(upload func(args map[string]any) error) error {
	path, err := buildAvatarPayload()
	if err != nil {
		return err
	}
	defer os.Remove(path)
	return upload(map[string]any{"archive_path": path, "mode": "layered", "timeout": avatarLoadTimeout})
}

// loadAvatarQuiet uploads the avatar set without writing to `out`, so the
// daemon can run it without holding execMu (an upload can take minutes).
func loadAvatarQuiet(c *mcp.Client) (text string, err error) {
	err = withAvatarPayload(func(args map[string]any) error {
		res, err := callToolQuiet(c, "load_avatar_set", args)
		text = res.Text()
		return err
	})
	return text, err
}

func cmdAvatarLoad(args []string, c *mcp.Client) error {
	set, verbose := fs("avatar-load")
	if err := set.Parse(args); err != nil {
		return err
	}
	return withAvatarPayload(func(toolArgs map[string]any) error {
		return callAndPrint(*verbose, true, "load_avatar_set", toolArgs, c)
	})
}
