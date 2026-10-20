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

// avatarDir is the directory of an avatar set: ~/.stackchan/avatar for the
// default set (name ""), ~/.stackchan/avatar-<name> for another (e.g. the
// sleep set, made by scripts/gen_avatars.py --sleep).
func avatarDir(name string) string {
	if name == "" {
		return filepath.Join(stackchanDir(), "avatar")
	}
	return filepath.Join(stackchanDir(), "avatar-"+name)
}

// validSetName keeps a set name to one path element under ~/.stackchan.
func validSetName(name string) bool {
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// buildAvatarPayload converts the PNGs into one raw little-endian RGB565 file
// (14 × 160×120) and returns its path; the caller removes it after the upload.
// Each call gets its own file so concurrent uploads (daemon reconnect + a
// manual avatar-load) never overwrite a file the gateway is serving.
// Images whose size is an integer multiple of 160×120 (e.g. 320×240) are
// box-downscaled. set names the avatar set (see avatarDir).
func buildAvatarPayload(set string) (string, error) {
	if !validSetName(set) {
		return "", fmt.Errorf("avatar set %q: use letters, digits, - and _ only", set)
	}
	dir := avatarDir(set)
	if _, err := os.Stat(dir); err != nil && set != "" {
		return "", fmt.Errorf("avatar set %q: %w (make the sleep set with scripts/gen_avatars.py --sleep)", set, err)
	}
	out := make([]byte, 0, len(avatarFrames)*avatarW*avatarH*2)
	for _, name := range avatarFrames {
		frame, err := loadAvatarFrame(filepath.Join(dir, name+".png"))
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
func withAvatarPayload(set string, upload func(args map[string]any) error) error {
	path, err := buildAvatarPayload(set)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	return upload(map[string]any{"archive_path": path, "mode": "layered", "timeout": avatarLoadTimeout})
}

// loadAvatarQuiet uploads an avatar set and returns the result text instead
// of printing it, for the daemon to log.
func loadAvatarQuiet(c *mcp.Client, set string) (text string, err error) {
	err = withAvatarPayload(set, func(args map[string]any) error {
		res, err := callTool(c, "load_avatar_set", args, nil)
		text = res.Text()
		return err
	})
	return text, err
}

func cmdAvatarLoad(args []string, e cmdEnv) error {
	set, verbose := fs("avatar-load")
	name := set.String("set", "", `avatar set to send: "" = ~/.stackchan/avatar, NAME = ~/.stackchan/avatar-NAME (e.g. sleep)`)
	if err := set.Parse(args); err != nil {
		return err
	}
	return withAvatarPayload(*name, func(toolArgs map[string]any) error {
		return callAndPrint(*verbose, true, "load_avatar_set", toolArgs, e)
	})
}
