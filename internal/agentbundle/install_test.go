package agentbundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// testArchive exercises the runtime boundary with local fixtures so failures cannot rely on external accounts.
func testArchive(t *testing.T, headers []*tar.Header) []byte {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tw := tar.NewWriter(gz)
	for _, h := range headers {
		// Fail the fixture when this invariant would weaken runtime isolation or verification.
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		// Fail the fixture when this invariant would weaken runtime isolation or verification.
		if h.Size > 0 {
			tw.Write(bytes.Repeat([]byte("x"), int(h.Size)))
		}
	}
	tw.Close()
	gz.Close()
	return data.Bytes()
}

// TestExtractRejectsUnsafeArchives exercises the runtime boundary with local fixtures so failures cannot rely on external accounts.
func TestExtractRejectsUnsafeArchives(t *testing.T) {
	for _, h := range []*tar.Header{{Name: "../outside", Typeflag: tar.TypeReg}, {Name: "/absolute", Typeflag: tar.TypeReg}, {Name: `C:\escape`, Typeflag: tar.TypeReg}, {Name: "linked", Typeflag: tar.TypeSymlink, Linkname: "../outside"}, {Name: "hard", Typeflag: tar.TypeLink, Linkname: "outside"}, {Name: "device", Typeflag: tar.TypeChar}} {
		t.Run(h.Name, func(t *testing.T) {
			// Fail the fixture when this invariant would weaken runtime isolation or verification.
			if err := extract(context.Background(), bytes.NewReader(testArchive(t, []*tar.Header{h})), t.TempDir(), 1024); err == nil {
				t.Fatal("accepted unsafe archive")
			}
		})
	}
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if err := extract(context.Background(), bytes.NewReader(testArchive(t, []*tar.Header{{Name: "large", Size: 32, Typeflag: tar.TypeReg}})), t.TempDir(), 8); err == nil {
		t.Fatal("accepted oversized archive")
	}
}

// TestInstallVerifiedCacheAndChecksum exercises the runtime boundary with local fixtures so failures cannot rely on external accounts.
func TestInstallVerifiedCacheAndChecksum(t *testing.T) {
	original := runtimeManifest
	defer func() { runtimeManifest = original }()
	archive := testArchive(t, []*tar.Header{{Name: "python/bin/python3.12", Typeflag: tar.TypeReg, Size: 1, Mode: 0755}, {Name: "packages/harnest/runtime.py", Typeflag: tar.TypeReg, Size: 1}})
	digest := sha256.Sum256(archive)
	sum := hex.EncodeToString(digest[:])
	entry := runtimeEntry{sum, "runtime-test.tar.gz", "python/bin/python3.12"}
	runtimeManifest, _ = json.Marshal(map[string]runtimeEntry{runtime.GOOS + "/" + runtime.GOARCH: entry})
	input := filepath.Join(t.TempDir(), "runtime.tar.gz")
	os.WriteFile(input, archive, 0600)
	options := Options{CacheDir: t.TempDir(), ArchivePath: input}
	first, err := InstallRuntime(context.Background(), options)
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(input)
	second, err := InstallRuntime(context.Background(), options)
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if err != nil || first != second {
		t.Fatalf("cache miss: %v", err)
	}
	options.CacheDir = t.TempDir()
	os.WriteFile(input, []byte("corrupt"), 0600)
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if _, err = InstallRuntime(context.Background(), options); err == nil {
		t.Fatal("accepted invalid checksum")
	}
}

// TestCacheLockCancellation exercises the runtime boundary with local fixtures so failures cannot rely on external accounts.
func TestCacheLockCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	unlock, err := lockCache(context.Background(), path)
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if _, err := lockCache(ctx, path); err == nil {
		t.Fatal("lock ignored cancellation")
	}
}

// TestEmbeddedAgentIsProductionArtifact exercises the runtime boundary with local fixtures so failures cannot rely on external accounts.
func TestEmbeddedAgentIsProductionArtifact(t *testing.T) {
	dir := t.TempDir()
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if err := extractAgent(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"launch.py", "harnest-manifest.json", "source/agent.py", "source/tools/get_page_context.py"} {
		// Fail the fixture when this invariant would weaken runtime isolation or verification.
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if _, err := os.Stat(filepath.Join(dir, "source/extensions")); !os.IsNotExist(err) {
		t.Fatal("demo SDK extensions included")
	}
}
