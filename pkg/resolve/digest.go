// Package resolve owns the deterministic ToolSpec resolution digests.
package resolve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	defaultBuilderIdentity = "nodevault:in-pod-buildah:podbridge5"
)

// Request is the NodeVault-internal representation of a ToolSpecRequest.
// RawSpec is preserved verbatim for audit, but digests use a canonical JSON
// form when RawSpec is valid JSON.
type Request struct {
	ToolName string
	Version  string
	RawSpec  string
}

// Context captures resolver inputs that are not authored by NodeKit but affect
// the resolved build output.
type Context struct {
	BuilderIdentity string
	BaseImageRef    string
	BaseImageDigest string
}

// Resolved is the deterministic NodeVault view of an authored ToolSpecRequest.
type Resolved struct {
	ToolSpecDigest     string
	RecipeInputsDigest string
	BuildPlanDigest    string
	BuilderIdentity    string
	BaseImageRef       string
	BaseImageDigest    string
}

// RecipeInputsDigest fingerprints the authored recipe payload. JSON payloads
// are canonicalized so field ordering and insignificant whitespace do not
// change the digest.
func RecipeInputsDigest(rawSpec string) (string, error) {
	canonical, err := canonicalSpec(rawSpec)
	if err != nil {
		return "", err
	}
	return sha256Hex([]byte("nodevault.recipe_inputs.v1\n" + canonical)), nil
}

// BuildPlanDigest fingerprints the build plan inputs controlled by NodeVault.
func BuildPlanDigest(recipeInputsDigest string, ctx Context) (string, error) {
	if strings.TrimSpace(recipeInputsDigest) == "" {
		return "", fmt.Errorf("recipeInputsDigest must not be empty")
	}
	builderIdentity := strings.TrimSpace(ctx.BuilderIdentity)
	if builderIdentity == "" {
		builderIdentity = defaultBuilderIdentity
	}
	payload, err := json.Marshal(struct {
		RecipeInputsDigest string `json:"recipe_inputs_digest"`
		BuilderIdentity    string `json:"builder_identity"`
		BaseImageDigest    string `json:"base_image_digest,omitempty"`
	}{
		RecipeInputsDigest: recipeInputsDigest,
		BuilderIdentity:    builderIdentity,
		BaseImageDigest:    strings.TrimSpace(ctx.BaseImageDigest),
	})
	if err != nil {
		return "", fmt.Errorf("marshal build plan digest payload: %w", err)
	}
	return sha256Hex(append([]byte("nodevault.build_plan.v1\n"), payload...)), nil
}

// Resolve derives deterministic digests and pinned metadata for a ToolSpecRequest.
func Resolve(req Request, ctx Context) (Resolved, error) {
	recipeDigest, err := RecipeInputsDigest(req.RawSpec)
	if err != nil {
		return Resolved{}, err
	}

	baseImageRef, baseImageDigest := BaseImagePin(req.RawSpec)
	if strings.TrimSpace(ctx.BaseImageRef) != "" {
		baseImageRef = strings.TrimSpace(ctx.BaseImageRef)
	}
	if strings.TrimSpace(ctx.BaseImageDigest) != "" {
		baseImageDigest = strings.TrimSpace(ctx.BaseImageDigest)
	}
	if baseImageDigest == "" {
		return Resolved{}, fmt.Errorf("base image must be pinned with @sha256 digest")
	}

	ctx.BaseImageDigest = baseImageDigest
	builderIdentity := strings.TrimSpace(ctx.BuilderIdentity)
	if builderIdentity == "" {
		builderIdentity = defaultBuilderIdentity
	}
	buildPlanDigest, err := BuildPlanDigest(recipeDigest, ctx)
	if err != nil {
		return Resolved{}, err
	}
	payload, err := json.Marshal(struct {
		ToolName           string `json:"tool_name"`
		Version            string `json:"version,omitempty"`
		RecipeInputsDigest string `json:"recipe_inputs_digest"`
		BuildPlanDigest    string `json:"build_plan_digest"`
		BaseImageDigest    string `json:"base_image_digest,omitempty"`
	}{
		ToolName:           strings.TrimSpace(req.ToolName),
		Version:            strings.TrimSpace(req.Version),
		RecipeInputsDigest: recipeDigest,
		BuildPlanDigest:    buildPlanDigest,
		BaseImageDigest:    baseImageDigest,
	})
	if err != nil {
		return Resolved{}, fmt.Errorf("marshal tool spec digest payload: %w", err)
	}
	return Resolved{
		ToolSpecDigest:     sha256Hex(append([]byte("nodevault.tool_spec.v1\n"), payload...)),
		RecipeInputsDigest: recipeDigest,
		BuildPlanDigest:    buildPlanDigest,
		BuilderIdentity:    builderIdentity,
		BaseImageRef:       baseImageRef,
		BaseImageDigest:    baseImageDigest,
	}, nil
}

// ToolSpecDigest fingerprints the full resolved spec boundary.
func ToolSpecDigest(req Request, ctx Context) (string, error) {
	resolved, err := Resolve(req, ctx)
	if err != nil {
		return "", err
	}
	return resolved.ToolSpecDigest, nil
}

func canonicalSpec(rawSpec string) (string, error) {
	trimmed := strings.TrimSpace(rawSpec)
	if trimmed == "" {
		return "", fmt.Errorf("rawSpec must not be empty")
	}

	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return trimmed, nil
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return "", fmt.Errorf("marshal canonical rawSpec: %w", err)
	}
	return string(canonical), nil
}

// BaseImagePin extracts a pinned base image digest from common raw_spec field names.
// It does not contact a registry; unpinned refs return an empty digest.
func BaseImagePin(rawSpec string) (baseImageRef, baseImageDigest string) {
	var decoded map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(rawSpec)), &decoded); err != nil {
		return "", ""
	}
	for _, key := range []string{"base_image", "base_image_uri", "image_uri"} {
		ref, ok := decoded[key].(string)
		if !ok {
			continue
		}
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		return ref, digestFromImageRef(ref)
	}
	return "", ""
}

func digestFromImageRef(ref string) string {
	idx := strings.LastIndex(ref, "@")
	if idx == -1 || idx == len(ref)-1 {
		return ""
	}
	digest := strings.TrimSpace(ref[idx+1:])
	if strings.HasPrefix(digest, "sha256:") {
		return digest
	}
	return ""
}

func sha256Hex(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
