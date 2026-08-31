package harnessworker

import (
	"context"

	"github.com/blazncloud/blazn/internal/sandboxio"
)

type ArtifactCollector interface {
	Collect(context.Context, []ArtifactSpec, bool) ([]ArtifactResult, []string, error)
}

type FileArtifactCollector struct{ Root string }

func (c FileArtifactCollector) Collect(ctx context.Context, specs []ArtifactSpec, enforceRequired bool) ([]ArtifactResult, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, protocolError("artifact_collection_cancelled")
	}
	root := c.Root
	if root == "" {
		root = DefaultArtifactRoot
	}
	fileSystem, err := sandboxio.OpenRootFileSystem(root)
	if err != nil {
		return nil, nil, protocolError("artifact_collection_failed")
	}
	defer fileSystem.Close()
	results := make([]ArtifactResult, 0, len(specs))
	warnings := make([]string, 0)
	var total int64
	for _, spec := range specs {
		if err := ctx.Err(); err != nil {
			return nil, nil, protocolError("artifact_collection_cancelled")
		}
		artifact, readErr := sandboxio.ReadArtifact(fileSystem, spec.Path, spec.MaxBytes)
		if sandboxio.IsProtocolError(readErr, "artifact_not_found") && (!spec.Required || !enforceRequired) {
			warnings = append(warnings, "optional_artifact_missing")
			continue
		}
		if readErr != nil || total > MaxTotalArtifactBytes-artifact.Size {
			return nil, nil, protocolError("artifact_collection_failed")
		}
		total += artifact.Size
		results = append(results, ArtifactResult{Name: spec.Name, Role: spec.Role, Kind: spec.Kind, MediaType: spec.MediaType, Size: artifact.Size, ContentDigest: artifact.SHA256})
		for index := range artifact.Body {
			artifact.Body[index] = 0
		}
	}
	return results, warnings, nil
}
