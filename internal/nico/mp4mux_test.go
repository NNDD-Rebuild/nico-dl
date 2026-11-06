package nico

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ffmpeg で作った fMP4 (映像/音声) を muxMP4 で結合し、デコードできること、
// ffmpeg -c copy の結果とパケット内容が一致することを確認する。
func TestMuxMP4(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg なし")
	}
	for _, rate := range []string{"48000", "44100"} {
		t.Run(rate, func(t *testing.T) {
			dir := t.TempDir()
			run := func(args ...string) string {
				cmd := exec.Command(ff, append([]string{"-v", "error", "-y"}, args...)...)
				cmd.Dir = dir
				b, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("ffmpeg %v: %v: %s", args, err, b)
				}
				return string(b)
			}
			run("-f", "lavfi", "-i", "testsrc=size=320x240:rate=30:duration=7", "-c:v", "libx264", "-bf", "2", "-g", "45", "-pix_fmt", "yuv420p",
				"-f", "hls", "-hls_time", "2", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "vinit.mp4", "-hls_segment_filename", "v%d.m4s", "v.m3u8")
			run("-f", "lavfi", "-i", "sine=frequency=440:sample_rate="+rate+":duration=7", "-c:a", "aac",
				"-f", "hls", "-hls_time", "2", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "ainit.mp4", "-hls_segment_filename", "a%d.m4s", "a.m3u8")
			concat := func(dst, init, pat string) {
				segs, _ := filepath.Glob(filepath.Join(dir, pat))
				sort.Strings(segs)
				out, _ := os.ReadFile(filepath.Join(dir, init))
				for _, s := range segs {
					b, _ := os.ReadFile(s)
					out = append(out, b...)
				}
				if err := os.WriteFile(filepath.Join(dir, dst), out, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			concat("video.mp4", "vinit.mp4", "v?.m4s")
			concat("audio.mp4", "ainit.mp4", "a?.m4s")

			out := filepath.Join(dir, "out.mp4")
			if err := muxMP4(context.Background(), filepath.Join(dir, "video.mp4"), filepath.Join(dir, "audio.mp4"), out); err != nil {
				t.Fatal(err)
			}
			run("-i", "out.mp4", "-f", "null", "-")
			run("-i", "video.mp4", "-i", "audio.mp4", "-c", "copy", "ref.mp4")

			packets := func(f string) string {
				cmd := exec.Command(ff, "-v", "error", "-i", f, "-map", "0", "-c", "copy", "-f", "framemd5", "-")
				cmd.Dir = dir
				b, err := cmd.Output()
				if err != nil {
					t.Fatal(err)
				}
				var lines []string
				for _, l := range strings.Split(string(b), "\n") {
					if l == "" || l[0] == '#' {
						continue
					}
					f := strings.Split(l, ",")
					lines = append(lines, f[0]+f[4]+f[5]) // stream, size, md5 (時刻は除外)
				}
				sort.Strings(lines)
				return strings.Join(lines, "\n")
			}
			if a, b := packets("ref.mp4"), packets("out.mp4"); a != b {
				t.Errorf("パケットが ffmpeg 結合と一致しない")
			}
		})
	}
}
