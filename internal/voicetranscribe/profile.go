package voicetranscribe

import "runtime"

// Artifact describes exact local bytes. Hashes of extracted engine members are
// local measurements of publisher-digest-verified archives, not publisher pins.
type Artifact struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Profile is trusted local configuration, never taken from an event or model
// output. Directory must contain bin/, lib/ and model/ below a physical root.
// The native closure is the pinned CLI + ONNX Runtime and audited OS libraries.
// The proposed Mac profile still requires actual native qualification.
type Profile struct {
	Directory   string   `json:"directory"`
	GOOS        string   `json:"goos"`
	GOARCH      string   `json:"goarch"`
	Executable  Artifact `json:"executable"`
	OnnxRuntime Artifact `json:"onnx_runtime"`
}

// PinnedProfile describes required files for a host. It neither creates nor
// qualifies an installation. Unsupported platforms return an unusable profile.
func PinnedProfile(directory string) Profile {
	p := Profile{Directory: directory, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	switch {
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		p.Executable = Artifact{"bin/sherpa-onnx-offline", 2509672, "95fb2157ba97830a57197129b19fbd93416b5c24be0ce5a23c47c834d8766aba"}
		p.OnnxRuntime = Artifact{"lib/libonnxruntime.so", 27026609, "4b3607aebd1784b26b6f9b20e4bd974c7ab8287043e4d095cb7d2cb40b5e566e"}
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		p.Executable = Artifact{"bin/sherpa-onnx-offline", 2051152, "23888c34222b5b47f4fe59d81ebd1bff1b1a683a01dcdc5ce4152892a23233b1"}
		p.OnnxRuntime = Artifact{"lib/libonnxruntime.dylib", 28775120, "3567d114f7299d559993e536d605a6f46d7bc9d2542004accc80ee9bf5457f0b"}
	}
	return p
}

func profileArtifacts(p Profile) []Artifact {
	want := PinnedProfile(p.Directory)
	if p.GOOS != want.GOOS || p.GOARCH != want.GOARCH || p.Executable != want.Executable ||
		p.OnnxRuntime != want.OnnxRuntime || p.Executable.Bytes == 0 {
		return nil
	}
	return []Artifact{p.Executable, p.OnnxRuntime}
}

func modelArtifacts() []Artifact {
	return []Artifact{
		{"model/tiny-encoder.int8.onnx", 12937772, "d24fb083ae3b1041fc24e97971d60e280c9342201fbb67b0ab428a8b4a51a434"},
		{"model/tiny-decoder.int8.onnx", 89855401, "d2fece8dd42771f1df975c6c0445770d0c292bf7547c2cae04a6c0cc57540925"},
		{"model/tiny-tokens.txt", 816730, "b34b360dbb493e781e479794586d661700670d65564001f23024971d1f2fa126"},
	}
}
