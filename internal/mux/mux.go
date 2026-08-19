// Package mux merges downloaded raw streams into the final container via ffmpeg.
package mux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mohamed/m314dl/internal/manifest"
)

type Input struct {
	Path     string
	Type     manifest.MediaType
	Language string
	Name     string
	Default  bool
}

// FindFFmpeg looks next to our binary, then PATH.
func FindFFmpeg() (string, error) {
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "ffmpeg")
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand, nil
		}
	}
	return exec.LookPath("ffmpeg")
}

// subtitle codec by output container
func subCodec(ext string) string {
	switch ext {
	case ".mp4", ".m4v", ".mov":
		return "mov_text"
	case ".mkv":
		return "srt"
	case ".webm":
		return "webvtt"
	}
	return ""
}

// Mux merges inputs into outPath (container from extension). Stream copy, no
// re-encode. Returns ffmpeg stderr tail on failure.
func Mux(ffmpeg string, inputs []Input, outPath string) error {
	if len(inputs) == 0 {
		return fmt.Errorf("mux: no inputs")
	}
	ext := strings.ToLower(filepath.Ext(outPath))
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	for _, in := range inputs {
		args = append(args, "-i", in.Path)
	}
	haveAudioInput := false
	for _, in := range inputs {
		if in.Type == manifest.Audio {
			haveAudioInput = true
		}
	}
	for i, in := range inputs {
		switch in.Type {
		case manifest.Audio:
			args = append(args, "-map", fmt.Sprintf("%d:a", i))
		case manifest.Video:
			if haveAudioInput {
				// separate audio chosen: drop any audio muxed into the
				// video variant to avoid duplicate tracks
				args = append(args, "-map", fmt.Sprintf("%d:v", i))
			} else {
				args = append(args, "-map", fmt.Sprint(i))
			}
		default:
			args = append(args, "-map", fmt.Sprint(i))
		}
	}
	// -dn: drop metadata/emsg data tracks CMAF inits often carry
	args = append(args, "-dn", "-c:v", "copy", "-c:a", "copy")
	if sc := subCodec(ext); sc != "" {
		args = append(args, "-c:s", sc)
	}
	ai, si := 0, 0
	for _, in := range inputs {
		switch in.Type {
		case manifest.Audio:
			if in.Language != "" {
				args = append(args, fmt.Sprintf("-metadata:s:a:%d", ai), "language="+lang639(in.Language))
			}
			if in.Default {
				args = append(args, fmt.Sprintf("-disposition:a:%d", ai), "default")
			}
			ai++
		case manifest.Subtitles:
			if in.Language != "" {
				args = append(args, fmt.Sprintf("-metadata:s:s:%d", si), "language="+lang639(in.Language))
			}
			if in.Name != "" {
				args = append(args, fmt.Sprintf("-metadata:s:s:%d", si), "title="+in.Name)
			}
			// Flag the first subtitle as default so a player turns it on without
			// the viewer having to pick a track.
			if si == 0 {
				args = append(args, fmt.Sprintf("-disposition:s:%d", si), "default")
			}
			si++
		}
	}
	if ext == ".mp4" || ext == ".m4v" || ext == ".mov" {
		args = append(args, "-movflags", "+faststart")
	}
	if ext == ".ts" {
		args = append(args, "-f", "mpegts")
	}
	args = append(args, outPath)
	cmd := exec.Command(ffmpeg, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		return fmt.Errorf("ffmpeg mux failed: %w\n%s", err, msg)
	}
	return nil
}

// ExtractSubtitle converts a raw subtitle stream file (fMP4 wvtt/stpp or
// anything ffmpeg understands) into outPath (.srt/.vtt by extension).
func ExtractSubtitle(ffmpeg, inPath, outPath string) error {
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", inPath, outPath)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg subtitle extract failed: %w\n%s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
