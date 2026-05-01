package oras_test

import (
	"context"
	"testing"

	"github.com/HeaInSeo/NodeVault/pkg/oras"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

func TestPushToolSpecReferrer_ValidationErrors(t *testing.T) {
	ctx := context.Background()
	validTool := &nfv1.RegisteredToolDefinition{
		ToolName:  "mytool",
		Version:   "1.0.0",
		StableRef: "mytool@1.0.0",
		ImageUri:  "harbor.example.com/library/mytool:latest",
		Digest:    "sha256:abc123",
		CasHash:   "deadbeef",
	}

	tests := []struct {
		name          string
		imageRepo     string
		subjectDigest string
		tool          *nfv1.RegisteredToolDefinition
		wantErrSubstr string
	}{
		{
			name:          "empty imageRepo",
			imageRepo:     "",
			subjectDigest: "sha256:abc",
			tool:          validTool,
			wantErrSubstr: "imageRepo must not be empty",
		},
		{
			name:          "empty subjectDigest",
			imageRepo:     "harbor.example.com/library/mytool",
			subjectDigest: "",
			tool:          validTool,
			wantErrSubstr: "subjectDigest must not be empty",
		},
		{
			name:          "nil tool",
			imageRepo:     "harbor.example.com/library/mytool",
			subjectDigest: "sha256:abc",
			tool:          nil,
			wantErrSubstr: "tool must not be nil",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := oras.PushToolSpecReferrer(ctx, tc.imageRepo, tc.subjectDigest, tc.tool)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if tc.wantErrSubstr != "" {
				msg := err.Error()
				if msg == "" {
					t.Fatalf("error message is empty")
				}
				found := false
				for i := 0; i <= len(msg)-len(tc.wantErrSubstr); i++ {
					if msg[i:i+len(tc.wantErrSubstr)] == tc.wantErrSubstr {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("error %q does not contain %q", msg, tc.wantErrSubstr)
				}
			}
		})
	}
}
