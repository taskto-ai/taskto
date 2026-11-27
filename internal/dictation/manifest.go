package dictation

import (
	"crypto/sha256"
	"encoding/hex"
	"runtime"
	"slices"
	"strings"
)

const (
	ModelParakeet = "parakeet-v3"
	ModelWhistle  = "whistle"
)

const (
	runSherpa = "sherpa-onnx"
	runCactus = "cactus"
)

var Models = []string{ModelParakeet, ModelWhistle}

type download struct {
	url     string
	sha256  string
	size    int64
	dest    string
	members map[string]string
}

type spec struct {
	id       string
	kind     string
	runtime  map[string][]download
	model    []download
	maxPiece int
	credits  []Credit
}

func (s spec) files(platform string) []download {
	rt, ok := s.runtime[platform]
	if !ok {
		return nil
	}
	return slices.Concat(rt, s.model)
}

func fingerprint(files []download) string {
	h := sha256.New()
	for _, f := range files {
		h.Write([]byte(f.sha256 + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

func sherpaOnnx(asset, sha string, size int64, lib string) []download {
	return []download{{
		url:    "https://github.com/k2-fsa/sherpa-onnx/releases/download/v2.0.2/sherpa-onnx-v2.0.2-" + asset + ".tar.bz2",
		sha256: sha, size: size,
		members: map[string]string{
			"bin/sherpa-onnx-offline": "bin/sherpa-onnx-offline",
			"lib/" + lib:              "lib/" + lib,
		},
	}}
}

func parakeetFile(name, sha string, size int64) download {
	return download{
		url:    "https://huggingface.co/csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8/resolve/ed5d745623472f97cfc2e6c2916ee248798fc5a8/" + name,
		sha256: sha, size: size, dest: name,
	}
}

func cactusRuntime(folder, sha string, size int64) []download {
	return []download{{
		url:    "https://huggingface.co/Cactus-Compute/cactus/resolve/1a028f5a3972b67dd8d8752f882113d475e7ec4c/releases/4.0.0/" + folder + "/cactus",
		sha256: sha, size: size, dest: "bin/cactus",
	}}
}

var specs = map[string]spec{
	ModelParakeet: {
		id: ModelParakeet, kind: runSherpa,
		runtime: map[string][]download{
			"linux/amd64":  sherpaOnnx("linux-x64-shared-no-tts", "c6725b8994a5e559678f72ddffbf1c14b41fc3f6f650618b98eeb61581b63da2", 27230798, "libonnxruntime.so"),
			"linux/arm64":  sherpaOnnx("linux-aarch64-shared-cpu", "5c715c43c8b2a9e591a7146f6a397419780dc2af78921ed9d36d993333317d43", 29462814, "libonnxruntime.so"),
			"darwin/arm64": sherpaOnnx("osx-arm64-shared-no-tts", "1a803809572335ef545978b62b0271ad119969fef6f9153a94c174c3adc95608", 20197239, "libonnxruntime.dylib"),
		},
		model: []download{
			parakeetFile("encoder.int8.onnx", "1afbe69c1ffdabee94755f42e4daf1d93549131a3da11356bda84fcef0f2c479", 652282294),
			parakeetFile("decoder.int8.onnx", "0222a86147299d4dee90edf95b6d13827edb42ad8b7c9cf0b2582f030937c0b5", 11845274),
			parakeetFile("joiner.int8.onnx", "cbd02f3696e9944970111fb439d9c3fd365a80374565f13f2b9c7007d253fc98", 6355277),
			parakeetFile("tokens.txt", "d58544679ea4bc6ac563d1f545eb7d474bd6cfa467f0a6e2c1dc1c7d37e3c35d", 93939),
		},
		maxPiece: 30,
		credits: []Credit{
			{
				Name: "Parakeet TDT 0.6B v3", By: "NVIDIA", URL: "https://huggingface.co/nvidia/parakeet-tdt-0.6b-v3",
				Licence: "CC-BY-4.0", LicenceURL: "https://creativecommons.org/licenses/by/4.0/",
				Note: "Speech recognition. Quantized to int8 and exported to ONNX by the sherpa-onnx project; not otherwise changed.",
			},
			{
				Name: "sherpa-onnx", By: "the k2-fsa project", URL: "https://github.com/k2-fsa/sherpa-onnx",
				Licence: "Apache-2.0", LicenceURL: "https://www.apache.org/licenses/LICENSE-2.0",
				Note: "Runs the local speech model, with ONNX Runtime (Microsoft, MIT).",
			},
		},
	},
	ModelWhistle: {
		id: ModelWhistle, kind: runCactus,
		runtime: map[string][]download{
			"linux/amd64":  cactusRuntime("linux-x86_64", "1233a27b65f2557ade0c965f17ba9de70a47e9f10c139539daca78814f946045", 1602448),
			"linux/arm64":  cactusRuntime("linux-arm64", "d1646246dca058cf240607d0f5344a910c7fa9de5e3d092ebf6a20e9d2810a45", 1482848),
			"darwin/arm64": cactusRuntime("macos-arm64", "39bd925a3759ad3dd18056918feffc82be43a92d4eb2ef5f8baf73a707ee3f3b", 1139720),
		},
		model: []download{{
			url:    "https://huggingface.co/Cactus-Compute/whistle/resolve/88cb00eeb8928ddbcd410b491584f938ba030b07/whistle.cact",
			sha256: "b6e02f048568ac5d01a2042556c658061e699acbc0aa2a1439f52f3d461dffeb", size: 16919407, dest: "whistle.cact",
		}},
		maxPiece: 20,
		credits: []Credit{
			{
				Name: "Whistle", By: "Cactus Compute", URL: "https://huggingface.co/Cactus-Compute/whistle",
				Licence: "Apache-2.0", LicenceURL: "https://www.apache.org/licenses/LICENSE-2.0",
				Note: "Speech recognition in English, German, French, Spanish, Italian, Dutch and Polish.",
			},
			{
				Name: "Cactus runtime 4.0", By: "Cactus Compute", URL: "https://github.com/cactus-compute/cactus",
				Licence: "Apache-2.0", LicenceURL: "https://www.apache.org/licenses/LICENSE-2.0",
				Note: "Runs the local speech model.",
			},
		},
	},
}

var whistleLanguages = strings.Fields("en de fr es it nl pl")
