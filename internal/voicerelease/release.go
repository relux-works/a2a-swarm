// Package voicerelease builds the relocatable macOS inference payload.
package voicerelease

import (
	"context"
	"debug/macho"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/voice"
)

const binding = "github.com/k2-fsa/sherpa-onnx-go-macos"
const bindingVersion = "v1.13.8"
const releaseRpath = "@executable_path/lib"

func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	b, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w\n%s", name, err, b)
	}
	return b, nil
}

func rpaths(file string) ([]string, error) {
	f, err := macho.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var result []string
	for _, load := range f.Loads {
		if r, ok := load.(*macho.Rpath); ok {
			result = append(result, r.Path)
		}
	}
	return result, nil
}

// AllowedLibrary enumerates the pinned payload's native references. No prefix
// grants permission to load an arbitrary library under a system-looking root.
func AllowedLibrary(name string) bool {
	switch name {
	case "@rpath/libonnxruntime.dylib", "@rpath/libsherpa-onnx-c-api.dylib",
		"/usr/lib/libSystem.B.dylib", "/usr/lib/libc++.1.dylib",
		"/usr/lib/libiconv.2.dylib", "/usr/lib/libresolv.9.dylib", "/usr/lib/libobjc.A.dylib",
		"/System/Library/Frameworks/CoreML.framework/Versions/A/CoreML",
		"/System/Library/Frameworks/Foundation.framework/Versions/C/Foundation",
		"/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation",
		"/System/Library/Frameworks/Security.framework/Versions/A/Security":
		return true
	}
	return false
}

func loadString(f *macho.File, b []byte, header int) (string, error) {
	if len(b) < header {
		return "", errors.New("short Mach-O load command")
	}
	offset := f.ByteOrder.Uint32(b[8:12])
	if offset < uint32(header) || uint64(offset) >= uint64(len(b)) {
		return "", errors.New("invalid Mach-O string offset")
	}
	tail := b[offset:]
	n := strings.IndexByte(string(tail), 0)
	if n < 0 {
		return "", errors.New("unterminated Mach-O load string")
	}
	return string(tail[:n]), nil
}

// Audit classifies EVERY command, including commands debug/macho leaves as raw
// bytes. Unrecognized loader directives fail closed instead of bypassing the
// dependency audit through ImportedLibraries' narrower view.
func Audit(file string, helper bool) error {
	f, err := macho.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	native := map[string]bool{}
	var rp []string
	ids := 0
	for _, load := range f.Loads {
		b := load.Raw()
		if len(b) < 8 || int(f.ByteOrder.Uint32(b[4:8])) != len(b) {
			return errors.New("invalid Mach-O load length")
		}
		kind := f.ByteOrder.Uint32(b[:4])
		switch kind {
		case 0xc: // LC_LOAD_DYLIB is the only admitted dependency command.
			name, err := loadString(f, b, 24)
			if err != nil || !AllowedLibrary(name) {
				return fmt.Errorf("unbundled native dependency: %q", name)
			}
			native[name] = true
		case 0xd: // LC_ID_DYLIB must identify this exact bundled library.
			name, err := loadString(f, b, 24)
			if err != nil || helper || name != "@rpath/"+filepath.Base(file) || !AllowedLibrary(name) {
				return errors.New("unexpected native library identity")
			}
			ids++
		case 0xe: // LC_LOAD_DYLINKER
			name, err := loadString(f, b, 12)
			if err != nil || !helper || name != "/usr/lib/dyld" {
				return errors.New("unexpected dynamic linker")
			}
		case 0x8000001c: // LC_RPATH
			name, err := loadString(f, b, 12)
			if err != nil {
				return err
			}
			rp = append(rp, name)
		case 0x1b: // LC_UUID
			if len(b) != 24 {
				return errors.New("invalid LC_UUID")
			}
		// Non-path commands emitted by the supported Go/Apple linker and the
		// pinned dylibs: segment64, symbol tables, dyld info/fixups/exports,
		// versions, entry point, function starts, data-in-code and signatures.
		case 0x19, 0x2, 0xb, 0x80000022, 0x80000034, 0x80000033,
			0x24, 0x32, 0x2a, 0x80000028, 0x26, 0x29, 0x1d:
		default:
			// Includes weak, re-export, upward, lazy, environment and unknown commands.
			return fmt.Errorf("unsupported Mach-O load command: %#x", kind)
		}
	}
	if helper {
		if !native["@rpath/libonnxruntime.dylib"] || !native["@rpath/libsherpa-onnx-c-api.dylib"] {
			return errors.New("helper must link both pinned native libraries")
		}
		if !hasUUID(f) {
			return errors.New("helper requires LC_UUID for native loader compatibility")
		}
		if len(rp) != 1 || rp[0] != releaseRpath {
			return errors.New("helper must have exactly @executable_path/lib rpath")
		}
	} else if len(rp) != 0 || ids != 1 {
		return errors.New("bundled library requires its own identity and no rpath")
	}
	return nil
}

func hasUUID(f *macho.File) bool {
	for _, load := range f.Loads {
		b := load.Raw()
		if len(b) == 24 && f.ByteOrder.Uint32(b[:4]) == 0x1b { // LC_UUID
			return true
		}
	}
	return false
}

// Build refuses existing destinations. A complete staged payload is published
// by rename only after linkage audit, deterministic signing and hashing.
func Build(ctx context.Context, dest string) (string, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return "", errors.New("voice release requires darwin/arm64")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		return "", errors.New("release destination must not exist")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(filepath.Dir(dest), ".voice-release-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	b, err := command(ctx, "go", "list", "-m", "-json", binding)
	if err != nil {
		return "", err
	}
	var module struct {
		Dir, Version string
		Replace      *json.RawMessage
	}
	if err := json.Unmarshal(b, &module); err != nil || module.Version != bindingVersion || module.Dir == "" || module.Replace != nil {
		return "", errors.New("unexpected Sherpa binding; require pinned v1.13.8 without replacement")
	}
	helper := filepath.Join(stage, voice.WorkerName)
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-tags", "sherpa", "-ldflags=-s -w -buildid= -extldflags=-Wl,-headerpad_max_install_names", "-o", helper, "./cmd/a2a-voice-worker")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1", "GOOS=darwin", "GOARCH=arm64")
	cmd.WaitDelay = time.Second
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("helper build failed: %w\n%s", err, b)
	}
	if err := os.Mkdir(filepath.Join(stage, "lib"), 0700); err != nil {
		return "", err
	}
	for _, name := range voice.ReleaseFiles()[1:] {
		src, err := os.Open(filepath.Join(module.Dir, "lib", "aarch64-apple-darwin", filepath.Base(name)))
		if err != nil {
			return "", err
		}
		dst, err := os.OpenFile(filepath.Join(stage, filepath.FromSlash(name)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			src.Close()
			return "", err
		}
		_, copyErr := io.Copy(dst, src)
		src.Close()
		closeErr := dst.Close()
		if copyErr != nil || closeErr != nil {
			return "", errors.New("copy native dependency failed")
		}
	}
	for _, name := range voice.ReleaseFiles() {
		file := filepath.Join(stage, filepath.FromSlash(name))
		rp, err := rpaths(file)
		if err != nil {
			return "", err
		}
		for _, r := range rp {
			if _, err := command(ctx, "install_name_tool", "-delete_rpath", r, file); err != nil {
				return "", err
			}
		}
		if name == voice.WorkerName {
			if _, err := command(ctx, "install_name_tool", "-add_rpath", releaseRpath, file); err != nil {
				return "", err
			}
		}
		if err := Audit(file, name == voice.WorkerName); err != nil {
			return "", err
		}
		if _, err := command(ctx, "codesign", "--force", "--sign", "-", "--timestamp=none", "--identifier", filepath.Base(name), file); err != nil {
			return "", err
		}
		if _, err := command(ctx, "codesign", "--verify", "--strict", file); err != nil {
			return "", err
		}
	}
	var manifest strings.Builder
	for _, name := range voice.ReleaseFiles() {
		sum, err := voice.HashReleaseFile(ctx, filepath.Join(stage, filepath.FromSlash(name)))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&manifest, "%s  %s\n", sum, name)
	}
	manifestPath := filepath.Join(stage, voice.ManifestName)
	if err := os.WriteFile(manifestPath, []byte(manifest.String()), 0600); err != nil {
		return "", err
	}
	sum, err := voice.HashReleaseFile(ctx, manifestPath)
	if err != nil {
		return "", err
	}
	if err := os.Rename(stage, dest); err != nil {
		return "", err
	}
	return sum, nil
}
