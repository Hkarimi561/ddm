package docker

import (
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
)

func TestNames(t *testing.T) {
	const dgst = "sha256:0123456789012345678901234567890123456789012345678901234567890123"
	tests := []struct {
		in, familiar, qualified string
	}{
		{"nginx", "nginx:latest", "docker.io/library/nginx:latest"},
		{"docker.io/library/nginx:1.27", "nginx:1.27", "docker.io/library/nginx:1.27"},
		{"bitnami/redis:7", "bitnami/redis:7", "docker.io/bitnami/redis:7"},
		{"ghcr.io/org/app:v1", "ghcr.io/org/app:v1", "ghcr.io/org/app:v1"},
		{"localhost:5000/app@" + dgst, "localhost:5000/app@" + dgst, "localhost:5000/app@" + dgst},
	}
	for _, tt := range tests {
		ref, err := name.ParseReference(tt.in)
		if err != nil {
			t.Fatalf("%s: %v", tt.in, err)
		}
		if got := FamiliarName(ref); got != tt.familiar {
			t.Errorf("FamiliarName(%s) = %s, want %s", tt.in, got, tt.familiar)
		}
		if got := QualifiedName(ref); got != tt.qualified {
			t.Errorf("QualifiedName(%s) = %s, want %s", tt.in, got, tt.qualified)
		}
	}
}
