package development

import (
	"bytes"
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gowebpki/jcs"
)

const (
	testBuildID    = "30000000-0000-4000-8000-000000000001"
	testArtifactID = "80000000-0000-4000-8000-000000000001"
)

func TestWriteEvidenceValidatesThenWritesCanonicalPrivateBundle(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "export")
	manifest := []byte(`{ "note": "safe", "artifactIds": ["` + testArtifactID + `"] }`)
	bundle := evidenceBundle{
		Manifest: manifest,
		Files: []evidenceFile{{
			ArtifactID:    testArtifactID,
			Path:          "artifacts/result.json",
			ContentBase64: base64.StdEncoding.EncodeToString([]byte(`{"ok":true}`)),
		}},
	}

	result, err := writeEvidence(target, testBuildID, bundle)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := jcs.Transform(manifest)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateFile(t, filepath.Join(target, evidenceManifestPath), canonical)
	assertPrivateFile(t, filepath.Join(target, "artifacts", "result.json"), []byte(`{"ok":true}`))
	if result.BuildID != testBuildID || result.Directory != target || len(result.ArtifactIDs) != 1 || result.ArtifactIDs[0] != testArtifactID {
		t.Fatalf("unexpected export: %#v", result)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("output directory mode=%v err=%v", info.Mode().Perm(), err)
	}
}

func TestWriteEvidenceRejectsAdversarialBundlesBeforeOutput(t *testing.T) {
	otherID := "80000000-0000-4000-8000-000000000002"
	valid := func() evidenceBundle {
		return evidenceBundle{
			Manifest: []byte(`{"artifactIds":["` + testArtifactID + `"]}`),
			Files:    []evidenceFile{{ArtifactID: testArtifactID, Path: "artifact.json", ContentBase64: base64.StdEncoding.EncodeToString([]byte(`{"ok":true}`))}},
		}
	}
	tests := map[string]func(*evidenceBundle){
		"manifest duplicate key": func(bundle *evidenceBundle) {
			bundle.Manifest = []byte(`{"artifactIds":["` + testArtifactID + `"],"artifactIds":["` + testArtifactID + `"]}`)
		},
		"manifest credential": func(bundle *evidenceBundle) {
			bundle.Manifest = []byte(`{"artifactIds":["` + testArtifactID + `"],"access_token":"redacted"}`)
		},
		"manifest bearer value": func(bundle *evidenceBundle) {
			bundle.Manifest = []byte(`{"artifactIds":["` + testArtifactID + `"],"note":"Bearer abcdefghijklmnop"}`)
		},
		"manifest signed URL value": func(bundle *evidenceBundle) {
			bundle.Manifest = []byte(`{"artifactIds":["` + testArtifactID + `"],"download":"https://example.test/object?X-Amz-Signature=abcdef"}`)
		},
		"manifest embedded signed URL in log text": func(bundle *evidenceBundle) {
			bundle.Manifest = []byte(`{"artifactIds":["` + testArtifactID + `"],"log":"download completed from https://account.blob.core.windows.net/container/object?sv=2024-11-04&sig=abcdef at 12:00"}`)
		},
		"manifest nested JSON text with signed URL": func(bundle *evidenceBundle) {
			bundle.Manifest = []byte(`{"artifactIds":["` + testArtifactID + `"],"log":"{\"result\":{\"url\":\"https://bucket.s3.example.test/object?X-Amz-Signature=abcdef\"}}"}`)
		},
		"manifest nested fully encoded signed URL": func(bundle *evidenceBundle) {
			encoded := url.QueryEscape(url.QueryEscape("https://bucket.s3.example.test/object?X-Amz-Signature=abcdef"))
			bundle.Manifest = []byte(`{"artifactIds":["` + testArtifactID + `"],"details":{"log":"download=` + encoded + `"}}`)
		},
		"duplicate artifact ID": func(bundle *evidenceBundle) {
			bundle.Manifest = []byte(`{"artifactIds":["` + testArtifactID + `","` + testArtifactID + `"]}`)
		},
		"missing manifest artifact": func(bundle *evidenceBundle) {
			bundle.Manifest = []byte(`{"artifactIds":["` + otherID + `"]}`)
		},
		"extra manifest artifact": func(bundle *evidenceBundle) {
			bundle.Manifest = []byte(`{"artifactIds":["` + testArtifactID + `","` + otherID + `"]}`)
		},
		"duplicate file ID": func(bundle *evidenceBundle) {
			bundle.Files = append(bundle.Files, evidenceFile{ArtifactID: testArtifactID, Path: "other", ContentBase64: "eA=="})
		},
		"duplicate path": func(bundle *evidenceBundle) {
			bundle.Manifest = []byte(`{"artifactIds":["` + testArtifactID + `","` + otherID + `"]}`)
			bundle.Files = append(bundle.Files, evidenceFile{ArtifactID: otherID, Path: "artifact.json", ContentBase64: "eA=="})
		},
		"manifest path collision": func(bundle *evidenceBundle) { bundle.Files[0].Path = evidenceManifestPath },
		"traversal":               func(bundle *evidenceBundle) { bundle.Files[0].Path = "../escape" },
		"absolute":                func(bundle *evidenceBundle) { bundle.Files[0].Path = "/escape" },
		"windows separator":       func(bundle *evidenceBundle) { bundle.Files[0].Path = `dir\escape` },
		"credential file": func(bundle *evidenceBundle) {
			bundle.Files[0].ContentBase64 = base64.StdEncoding.EncodeToString([]byte(`{"password":"redacted"}`))
		},
		"plaintext credential file": func(bundle *evidenceBundle) {
			bundle.Files[0].ContentBase64 = base64.StdEncoding.EncodeToString([]byte(`authorization=super-secret-value`))
		},
		"embedded signed URL in plaintext file": func(bundle *evidenceBundle) {
			bundle.Files[0].ContentBase64 = base64.StdEncoding.EncodeToString([]byte(`download URL: https://account.blob.core.windows.net/container/object?sv=2024-11-04&sig=abcdef status=ready`))
		},
		"fully encoded signed URL in plaintext file": func(bundle *evidenceBundle) {
			encoded := url.QueryEscape("https://bucket.s3.example.test/object?X-Amz-Signature=abcdef")
			bundle.Files[0].ContentBase64 = base64.StdEncoding.EncodeToString([]byte("download URL: " + encoded + " status=ready"))
		},
		"over-depth encoded signed URL fails closed": func(bundle *evidenceBundle) {
			encoded := "https://bucket.s3.example.test/object?X-Amz-Signature=abcdef"
			for range 6 {
				encoded = url.QueryEscape(encoded)
			}
			bundle.Files[0].ContentBase64 = base64.StdEncoding.EncodeToString([]byte("download URL: " + encoded + " status=ready"))
		},
		"malformed escape in plaintext file": func(bundle *evidenceBundle) {
			bundle.Files[0].ContentBase64 = base64.StdEncoding.EncodeToString([]byte("download URL: https%3A%2F%2Fexample.test%2Fobject%3Fsig%ZZsecret"))
		},
		"invalid base64": func(bundle *evidenceBundle) { bundle.Files[0].ContentBase64 = "%%%" },
		"oversize file": func(bundle *evidenceBundle) {
			bundle.Files[0].ContentBase64 = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, maxEvidenceFileBytes+1))
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			bundle := valid()
			mutate(&bundle)
			target := filepath.Join(t.TempDir(), "must-not-exist")
			if _, err := writeEvidence(target, testBuildID, bundle); err == nil {
				t.Fatal("adversarial bundle accepted")
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("output created before validation: %v", err)
			}
		})
	}
}

func TestValidateEvidenceRejectsExcessiveCountsAndAggregateDecodedSize(t *testing.T) {
	t.Run("file count", func(t *testing.T) {
		bundle := evidenceBundle{Manifest: []byte(`{"artifactIds":[]}`)}
		bundle.Files = make([]evidenceFile, maxEvidenceFiles+1)
		if _, err := validateEvidence(bundle); err == nil {
			t.Fatal("excessive file count accepted")
		}
	})
	t.Run("artifact count", func(t *testing.T) {
		ids := make([]string, maxEvidenceFiles+1)
		for index := range ids {
			ids[index] = `"80000000-0000-4000-8000-` + strings.Repeat("0", 9) + string(rune('a'+index%6)) + `"`
		}
		bundle := evidenceBundle{Manifest: []byte(`{"artifactIds":[` + strings.Join(ids, ",") + `]}`), Files: []evidenceFile{{}}}
		if _, err := validateEvidence(bundle); err == nil {
			t.Fatal("excessive artifact count accepted")
		}
	})
	t.Run("aggregate decoded size", func(t *testing.T) {
		encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, maxEvidenceFileBytes))
		ids := make([]string, 9)
		files := make([]evidenceFile, 9)
		for index := range ids {
			id := "80000000-0000-4000-8000-00000000000" + string(rune('1'+index))
			ids[index] = `"` + id + `"`
			files[index] = evidenceFile{ArtifactID: id, Path: "artifact-" + string(rune('a'+index)), ContentBase64: encoded}
		}
		bundle := evidenceBundle{Manifest: []byte(`{"artifactIds":[` + strings.Join(ids, ",") + `]}`), Files: files}
		if _, err := validateEvidence(bundle); err == nil {
			t.Fatal("excessive aggregate decoded size accepted")
		}
	})
}

func TestWriteEvidenceRefusesExistingAndSymlinkedTargets(t *testing.T) {
	bundle := evidenceBundle{
		Manifest: []byte(`{"artifactIds":["` + testArtifactID + `"]}`),
		Files:    []evidenceFile{{ArtifactID: testArtifactID, Path: "artifact", ContentBase64: "eA=="}},
	}
	parent := t.TempDir()
	existing := filepath.Join(parent, "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(existing, "keep")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"existing": existing,
		"symlink":  filepath.Join(parent, "link"),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "symlink" {
				if err := os.Symlink(existing, target); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := writeEvidence(target, testBuildID, bundle); err == nil {
				t.Fatal("unsafe target accepted")
			}
			content, err := os.ReadFile(marker)
			if err != nil || string(content) != "keep" {
				t.Fatalf("pre-existing path changed: %q %v", content, err)
			}
		})
	}
}

func TestWriteEvidenceRefusesSymlinkedParentComponent(t *testing.T) {
	bundle := evidenceBundle{
		Manifest: []byte(`{"artifactIds":["` + testArtifactID + `"]}`),
		Files:    []evidenceFile{{ArtifactID: testArtifactID, Path: "artifact", ContentBase64: "eA=="}},
	}
	realParent := t.TempDir()
	linkedParent := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	if _, err := writeEvidence(filepath.Join(linkedParent, "export"), testBuildID, bundle); err == nil {
		t.Fatal("symlinked parent component accepted")
	}
	if _, err := os.Lstat(filepath.Join(realParent, "export")); !os.IsNotExist(err) {
		t.Fatalf("output escaped through symlinked parent: %v", err)
	}
}

func TestWriteEvidenceCleansOnlyInvocationPathsAfterWriteFailure(t *testing.T) {
	otherID := "80000000-0000-4000-8000-000000000002"
	parent := t.TempDir()
	keep := filepath.Join(parent, "keep")
	if err := os.WriteFile(keep, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "export")
	bundle := evidenceBundle{
		Manifest: []byte(`{"artifactIds":["` + testArtifactID + `","` + otherID + `"]}`),
		Files: []evidenceFile{
			{ArtifactID: testArtifactID, Path: "collision", ContentBase64: "eA=="},
			{ArtifactID: otherID, Path: "collision/child", ContentBase64: "eQ=="},
		},
	}
	if _, err := writeEvidence(target, testBuildID, bundle); err == nil {
		t.Fatal("expected write failure")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("invocation output was not cleaned: %v", err)
	}
	content, err := os.ReadFile(keep)
	if err != nil || string(content) != "unchanged" {
		t.Fatalf("unrelated path changed: %q %v", content, err)
	}
}

func assertPrivateFile(t *testing.T, name string, expected []byte) {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, expected) {
		t.Fatalf("%s content=%q want=%q", name, content, expected)
	}
	info, err := os.Stat(name)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s mode=%v err=%v", name, info.Mode().Perm(), err)
	}
}

func TestSafeEvidencePathRejectsPlatformAmbiguity(t *testing.T) {
	for _, value := range []string{"", ".", "..", "a/../b", "a//b", `/absolute`, `C:\\absolute`, "manifest.json", strings.Repeat("x", 513)} {
		if safeEvidencePath(value) && value != evidenceManifestPath {
			t.Fatalf("unsafe path accepted: %q", value)
		}
	}
}
