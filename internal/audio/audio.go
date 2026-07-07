// Package audio is the storage + conversion engine for admin-uploaded
// audio clips. Clips are played by Asterisk when a DID is reserved with
// route_kind='audio'; the dialplan does Answer() + Playback(basename) +
// Hangup() and the file under SoundsDir is what gets played.
//
// Why we always convert to .slin (16-bit signed-linear 8 kHz mono):
//
//   - Inbound carrier legs are G.711 ulaw/alaw or g722. Asterisk transcodes
//     transparently from slin to any of those, so storing slin means every
//     call works regardless of negotiated codec.
//   - slin is uncompressed and lossless — re-encoding to ulaw at playback
//     time costs one transcode step per call and is faster than reading +
//     decoding mp3/m4a/ogg every time.
//   - It avoids surprises with Asterisk's automatic ".wav" interpretation
//     (which is sample-rate dependent and will refuse to play 44.1kHz wav
//     files into an 8kHz call).
//
// Conversion prefers ffmpeg (most installs have it) and falls back to sox.
// Both are commonly available on Debian; ffmpeg handles more input formats
// out of the box (m4a/aac, ogg/opus). The conversion is bounded to 60s
// real-time to defend against pathological inputs.
package audio

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// SoundsDir is where the canonical .slin files live on the server.
// extensions.conf has Playback(didstorage/<basename>); Asterisk resolves
// that against /var/lib/asterisk/sounds/<dir>/<basename>.<ext>.
//
// Overridable so tests / dev boxes can point somewhere writable without
// running as root. Default matches the server install.
var SoundsDir = "/var/lib/asterisk/sounds/didstorage"

// PlaybackPrefix is the path prefix Asterisk's Playback() command expects,
// relative to its sounds root. Combined with the file basename it forms
// the argument: Playback(didstorage/af_abc123).
const PlaybackPrefix = "didstorage"

// MaxUploadBytes guards against accidental huge uploads — 50 MB is roughly
// 8 hours of slin or a 50 MB mp3 (~50 min at 128kbps). Anything longer is
// almost certainly a mistake and isn't a sensible "play and hang up" clip.
const MaxUploadBytes = 50 * 1024 * 1024

// AllowedInputExts is the set of upload extensions we'll try to convert.
// Anything else gets rejected up front before we burn ffmpeg time on it.
var AllowedInputExts = map[string]struct{}{
	".mp3":  {},
	".wav":  {},
	".m4a":  {},
	".aac":  {},
	".ogg":  {},
	".opus": {},
	".flac": {},
	".webm": {},
	".slin": {},
	".sln":  {},
	".ulaw": {},
	".alaw": {},
	".gsm":  {},
}

// ConvertResult is what Convert returns: where the canonical file lives,
// its on-disk size, and how long it plays for (ms, as detected by ffprobe
// or sox). Duration is best-effort; 0 means we couldn't probe it.
type ConvertResult struct {
	Filename   string // basename without extension, e.g. "af_abc12345"
	Path       string // absolute path on disk, including ".slin" extension
	SizeBytes  int64
	DurationMS int
	Format     string // always "slin" today
}

// NewFilename returns a fresh basename guaranteed to be unique in SoundsDir.
// 16 hex chars of randomness gives a vanishingly low collision risk; we
// retry on the off-chance one happens.
func NewFilename() (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var buf [8]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return "", err
		}
		name := "af_" + hex.EncodeToString(buf[:])
		if _, err := os.Stat(filepath.Join(SoundsDir, name+".slin")); errors.Is(err, os.ErrNotExist) {
			return name, nil
		}
	}
	return "", errors.New("audio: failed to allocate unique filename after retries")
}

// SafeOriginalExt returns the lowercased extension of the upload filename
// or "" if missing / not allowed. Caller uses this to validate uploads
// before invoking Convert.
func SafeOriginalExt(orig string) string {
	ext := strings.ToLower(filepath.Ext(orig))
	if _, ok := AllowedInputExts[ext]; !ok {
		return ""
	}
	return ext
}

// EnsureDir creates SoundsDir with permissions Asterisk can read. Called
// once at startup so missing-directory bugs surface immediately, not on
// the first upload.
func EnsureDir() error {
	if err := os.MkdirAll(SoundsDir, 0o755); err != nil {
		return fmt.Errorf("audio: mkdir %s: %w", SoundsDir, err)
	}
	return nil
}

// Convert reads `src` (a temp file already on disk, in any supported
// format), normalises it to slin 8 kHz mono 16-bit signed, and writes
// the result to SoundsDir/<basename>.slin. Returns the canonical basename
// + duration + size. The src file is left in place — caller cleans it up.
//
// We prefer ffmpeg over sox because ffmpeg's input format support is
// broader (m4a/aac/opus/webm). Both produce identical output: raw 16-bit
// little-endian PCM with no container, which Asterisk's "slin" format
// reader expects byte-for-byte.
func Convert(ctx context.Context, srcPath string) (*ConvertResult, error) {
	if err := EnsureDir(); err != nil {
		return nil, err
	}
	basename, err := NewFilename()
	if err != nil {
		return nil, err
	}
	outPath := filepath.Join(SoundsDir, basename+".slin")

	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	if err := convertWithFFmpeg(cctx, srcPath, outPath); err != nil {
		// ffmpeg may simply not be installed — fall back to sox before
		// giving up. If sox also fails, surface the ffmpeg error since
		// it's the more informative one for the operator.
		if soxErr := convertWithSox(cctx, srcPath, outPath); soxErr != nil {
			_ = os.Remove(outPath)
			return nil, fmt.Errorf("audio: ffmpeg failed (%v) and sox failed (%v)", err, soxErr)
		}
	}

	info, err := os.Stat(outPath)
	if err != nil {
		return nil, fmt.Errorf("audio: stat output: %w", err)
	}
	if info.Size() == 0 {
		_ = os.Remove(outPath)
		return nil, errors.New("audio: converter produced empty file")
	}

	// slin = 16-bit signed @ 8000 Hz mono = 16000 bytes/sec.
	// Use that for duration so we don't need a second probe call. If we
	// ever support other sample rates this gets a switch.
	const bytesPerSec = 16000
	durationMS := int(info.Size() * 1000 / bytesPerSec)

	// chmod 644 so the asterisk user can read regardless of the umask
	// inherited from the calling process.
	_ = os.Chmod(outPath, 0o644)

	return &ConvertResult{
		Filename:   basename,
		Path:       outPath,
		SizeBytes:  info.Size(),
		DurationMS: durationMS,
		Format:     "slin",
	}, nil
}

func convertWithFFmpeg(ctx context.Context, src, dst string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg not in PATH: %w", err)
	}
	// -y: overwrite output without prompting (we picked a unique name, but
	//     belt-and-braces in case of retry).
	// -i: input.
	// -ar 8000: resample to 8 kHz.
	// -ac 1: mono.
	// -f s16le: raw 16-bit little-endian PCM (no container) — Asterisk slin.
	// -acodec pcm_s16le: explicit codec, matches container format.
	// -nostdin -hide_banner -loglevel error: quiet, non-interactive.
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-y",
		"-i", src,
		"-ar", "8000",
		"-ac", "1",
		"-f", "s16le",
		"-acodec", "pcm_s16le",
		dst,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func convertWithSox(ctx context.Context, src, dst string) error {
	if _, err := exec.LookPath("sox"); err != nil {
		return fmt.Errorf("sox not in PATH: %w", err)
	}
	// sox input output options
	//   -r 8000: target sample rate
	//   -c 1: mono
	//   -b 16: 16-bit
	//   -e signed-integer: signed-int encoding
	//   -t raw: no container
	cmd := exec.CommandContext(ctx, "sox",
		src,
		"-r", "8000",
		"-c", "1",
		"-b", "16",
		"-e", "signed-integer",
		"-t", "raw",
		dst,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sox: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Delete removes the on-disk file for a stored audio. Missing files are
// not an error — the DB row may have outlived a manual `rm` and we still
// want to be able to drop the row.
func Delete(filename string) error {
	if filename == "" {
		return nil
	}
	if !isSafeBasename(filename) {
		return fmt.Errorf("audio: refusing to delete unsafe filename %q", filename)
	}
	path := filepath.Join(SoundsDir, filename+".slin")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("audio: delete %s: %w", path, err)
	}
	return nil
}

// Open returns a read-closer for the canonical file. Used by the playback
// HTTP handler to stream the clip to the browser. Caller closes.
func Open(filename string) (io.ReadCloser, int64, error) {
	if !isSafeBasename(filename) {
		return nil, 0, fmt.Errorf("audio: unsafe filename %q", filename)
	}
	path := filepath.Join(SoundsDir, filename+".slin")
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// PlaybackTarget is the value the AGI returns to the dialplan as
// AUTH_ROUTE_TARGET when route_kind='audio'. The dialplan wraps it with
// PlaybackPrefix when calling Playback(). Kept as a function so the
// prefix-shaping logic lives in one place.
func PlaybackTarget(filename string) string {
	return PlaybackPrefix + "/" + filename
}

// FormatDuration renders a ms duration as "MM:SS" or "M:SS.s" for short
// clips, suitable for table display. Returns "—" for zero.
func FormatDuration(ms int) string {
	if ms <= 0 {
		return "—"
	}
	if ms < 60000 {
		whole := ms / 1000
		tenths := (ms % 1000) / 100
		return strconv.Itoa(whole) + "." + strconv.Itoa(tenths) + "s"
	}
	totalSec := ms / 1000
	m := totalSec / 60
	s := totalSec % 60
	if s < 10 {
		return strconv.Itoa(m) + ":0" + strconv.Itoa(s)
	}
	return strconv.Itoa(m) + ":" + strconv.Itoa(s)
}

// isSafeBasename rejects anything that could escape SoundsDir.
// Constraints match the audio_files_filename_safe CHECK in migration 0016.
func isSafeBasename(s string) bool {
	if s == "" || len(s) > 96 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}
