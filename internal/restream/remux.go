package restream

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/mohamed/m314dl/internal/engine"
)

// Cross-container remux to MPEG-TS via one long-lived FFmpeg.
//
// The pure-Go TS path (mpegts.go) only works when the source is already a muxed
// transport stream. When it isn't — an fMP4/CMAF source, or separate video and
// audio that must be muxed together — RemuxTS bridges the gap: it feeds each
// ordered, decrypted input track into FFmpeg over its own pipe and reads one
// continuous MPEG-TS off FFmpeg's stdout into the broadcaster.
//
// This is the deliberate, bounded use of FFmpeg the rest of the engine avoids,
// and it keeps the properties that made the pure path good:
//   - ONE process for the whole stream, not one per segment (the drm worker's
//     per-segment spawn anti-pattern);
//   - NO -re pacing — input arrives at the source's own segment cadence, so the
//     output is paced without a wall-clock throttle;
//   - FFmpeg only ever sees an already-decrypted, clear copy, so the mov-demuxer
//     CENC leak that forced the worker to decrypt upstream never applies here.
//
// Default is stream copy (remux only). An explicit transcode arg list replaces
// the copy codecs when a real re-encode is asked for.

const remuxReadChunk = 256 << 10 // read FFmpeg stdout in ~256KB chunks

// RemuxTS runs one FFmpeg that muxes N input tracks into a broadcast TS.
type RemuxTS struct {
	b      *TSBroadcaster
	cmd    *exec.Cmd
	inputs []*os.File // parent write ends, one per track (child reads pipe:3..)
	stderr *strings.Builder
	logv   func(string, ...any)
	closed sync.Once
}

// NewRemuxTS starts the FFmpeg remuxer. ntracks input pipes are created and
// exposed as pipe:3, pipe:4, … to the child; transcodeArgs (nil = stream copy)
// replaces the output codec args.
func NewRemuxTS(ffmpeg string, ntracks int, transcodeArgs []string, b *TSBroadcaster, logv func(string, ...any)) (*RemuxTS, error) {
	if ntracks < 1 {
		return nil, fmt.Errorf("remux: no input tracks")
	}
	if logv == nil {
		logv = func(string, ...any) {}
	}

	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	var extra []*os.File // child-side read ends, become fd 3,4,…
	var inputs []*os.File
	for i := 0; i < ntracks; i++ {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		extra = append(extra, r)
		inputs = append(inputs, w)
		// -thread_queue_size well above FFmpeg's default of 8: with several input
		// demuxers reading concurrently the default queue fills, FFmpeg logs
		// "Thread message queue blocking", and can stall — the pipe writers then
		// block with it. A deeper queue absorbs bursty per-input arrival.
		args = append(args, "-thread_queue_size", "512", "-i", fmt.Sprintf("pipe:%d", 3+i))
	}
	for i := 0; i < ntracks; i++ {
		args = append(args, "-map", fmt.Sprint(i))
	}
	if len(transcodeArgs) > 0 {
		args = append(args, transcodeArgs...)
	} else {
		args = append(args, "-c", "copy")
	}
	// Bound the muxer backlog so a lagging input can't grow memory without limit
	// on endless live input (the worker's -shortest never fires there either).
	args = append(args, "-max_muxing_queue_size", "1024", "-f", "mpegts", "pipe:1")

	cmd := exec.Command(ffmpeg, args...)
	cmd.ExtraFiles = extra // r[0]→fd3, r[1]→fd4, …
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg remux: %w", err)
	}
	// The child holds its own dups of the read ends; the parent must close them
	// so an input pipe reaches EOF once we close its write end.
	for _, r := range extra {
		r.Close()
	}

	rm := &RemuxTS{b: b, cmd: cmd, inputs: inputs, stderr: &stderr, logv: logv}
	go rm.pump(stdout)
	return rm, nil
}

// pump reads the remuxed TS off FFmpeg and fans it out until FFmpeg exits.
func (rm *RemuxTS) pump(stdout io.ReadCloser) {
	buf := make([]byte, remuxReadChunk)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			rm.b.publish(chunk)
		}
		if err != nil {
			break
		}
	}
	// FFmpeg output ended: no more segments will be broadcast.
	rm.b.End()
	if err := rm.cmd.Wait(); err != nil {
		if msg := strings.TrimSpace(rm.stderr.String()); msg != "" {
			rm.logv("ffmpeg remux exited: %v: %s", err, lastLine(msg))
		} else {
			rm.logv("ffmpeg remux exited: %v", err)
		}
	}
}

// Sink returns the engine.Sink that feeds input track i.
func (rm *RemuxTS) Sink(i int) engine.Sink { return remuxTrackSink{w: rm.inputs[i]} }

// End closes the input pipes so FFmpeg drains and exits; pump then ends the
// broadcaster. Safe to call more than once. Implements the presentation
// contract used by the restream run loop.
func (rm *RemuxTS) End() {
	rm.closed.Do(func() {
		for _, w := range rm.inputs {
			w.Close()
		}
	})
}

func (rm *RemuxTS) StatusLine() string { return rm.b.StatusLine() }

// remuxTrackSink writes one track's init + segments into its FFmpeg input pipe.
// For fMP4 the init (ftyp+moov) is written first, then fragments — exactly a
// fragmented-MP4 byte stream, which FFmpeg demuxes from the pipe.
type remuxTrackSink struct{ w io.Writer }

func (s remuxTrackSink) Init(data []byte) error {
	_, err := s.w.Write(data)
	return err
}

func (s remuxTrackSink) Segment(_ engine.SegmentInfo, data []byte) error {
	_, err := s.w.Write(data)
	return err
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(strings.TrimRight(s, "\n"), '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
