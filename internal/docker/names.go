package docker

import (
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// FamiliarName returns the name Docker shows for ref, e.g. "nginx:latest"
// for index.docker.io/library/nginx:latest.
func FamiliarName(ref name.Reference) string {
	repo := ref.Context()
	n := repo.RegistryStr() + "/" + repo.RepositoryStr()
	if repo.RegistryStr() == name.DefaultRegistry {
		n = strings.TrimPrefix(repo.RepositoryStr(), "library/")
	}
	return n + suffix(ref)
}

// QualifiedName returns the fully qualified name containerd uses, e.g.
// "docker.io/library/nginx:latest".
func QualifiedName(ref name.Reference) string {
	repo := ref.Context()
	reg := repo.RegistryStr()
	if reg == name.DefaultRegistry {
		reg = "docker.io"
	}
	return reg + "/" + repo.RepositoryStr() + suffix(ref)
}

func suffix(ref name.Reference) string {
	switch r := ref.(type) {
	case name.Tag:
		return ":" + r.TagStr()
	case name.Digest:
		return "@" + r.DigestStr()
	}
	return ""
}
