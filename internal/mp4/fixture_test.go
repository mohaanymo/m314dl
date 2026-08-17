package mp4

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fixtureDir points at shaka-packaged CENC/CBCS test streams. Override with
// M314_FIXTURES. The test skips when the fixtures or mp4decrypt are absent, so
// it never blocks CI but proves byte-exact parity locally.
func fixtureDir() string {
	if d := os.Getenv("M314_FIXTURES"); d != "" {
		return d
	}
	return "../../bench/fixtures" // produced by bench/gen-fixtures.sh
}

func mdatOf(buf []byte) []byte {
	var off int64
	for off+8 <= int64(len(buf)) {
		size := int64(binary.BigEndian.Uint32(buf[off:]))
		typ := string(buf[off+4 : off+8])
		hdr := int64(8)
		switch size {
		case 1:
			size = int64(binary.BigEndian.Uint64(buf[off+8:]))
			hdr = 16
		case 0:
			size = int64(len(buf)) - off
		}
		if typ == "mdat" {
			return buf[off+hdr : off+size]
		}
		off += size
	}
	return nil
}

// TestDecryptParityWithMp4decrypt decrypts real shaka fragments natively and
// asserts the resulting mdat is byte-identical to mp4decrypt's.
func TestDecryptParityWithMp4decrypt(t *testing.T) {
	dir := fixtureDir()
	if _, err := os.Stat(filepath.Join(dir, "dash-cenc", "v_init.mp4")); err != nil {
		t.Skip("CENC fixtures not present; skipping parity test")
	}
	mp4decrypt, err := exec.LookPath("mp4decrypt")
	if err != nil {
		t.Skip("mp4decrypt not on PATH; skipping parity test")
	}
	const kid = "00112233445566778899aabbccddeeff"
	const keyHex = "0123456789abcdef0123456789abcdef"
	key, _ := hex.DecodeString(keyHex)

	for _, scheme := range []string{"dash-cenc", "dash-cbcs"} {
		for _, tr := range []string{"v", "a"} {
			name := scheme + "/" + tr
			t.Run(name, func(t *testing.T) {
				sd := filepath.Join(dir, scheme)
				initSeg, err := os.ReadFile(filepath.Join(sd, tr+"_init.mp4"))
				if err != nil {
					t.Fatal(err)
				}
				info, err := ParseInit(initSeg)
				if err != nil {
					t.Fatal(err)
				}
				if info == nil {
					t.Fatal("init not detected as encrypted")
				}
				frag, err := os.ReadFile(filepath.Join(sd, tr+"_2.m4s"))
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("scheme=%s ivlen=%d crypt:skip=%d:%d constIV=%d protected=%v",
					info.Scheme, info.PerSampleIVLen, info.CryptByteBlock, info.SkipByteBlock, len(info.ConstantIV), info.Protected)
				mine := append([]byte(nil), frag...)
				if err := DecryptFragment(mine, info, key); err != nil {
					t.Fatalf("native decrypt: %v", err)
				}

				in := filepath.Join(t.TempDir(), "in.mp4")
				out := filepath.Join(t.TempDir(), "out.mp4")
				if err := os.WriteFile(in, append(append([]byte(nil), initSeg...), frag...), 0o644); err != nil {
					t.Fatal(err)
				}
				if b, err := exec.Command(mp4decrypt, "--key", kid+":"+keyHex, in, out).CombinedOutput(); err != nil {
					t.Fatalf("mp4decrypt: %v: %s", err, b)
				}
				refBuf, err := os.ReadFile(out)
				if err != nil {
					t.Fatal(err)
				}
				ref, got := mdatOf(refBuf), mdatOf(mine)
				if len(ref) != len(got) {
					t.Fatalf("mdat length mine=%d ref=%d (scheme=%s)", len(got), len(ref), info.Scheme)
				}
				if !bytes.Equal(ref, got) {
					n := 0
					for n < len(ref) && ref[n] == got[n] {
						n++
					}
					t.Fatalf("mdat mismatch at byte %d/%d (scheme=%s)", n, len(ref), info.Scheme)
				}
				t.Logf("byte-exact vs mp4decrypt: %d mdat bytes, scheme=%s", len(got), info.Scheme)
			})
		}
	}
}
