// Package modelref parses model references the way Docker Model Runner
// does, so ddm stores models under exactly the names `docker model` looks
// for.
package modelref

import (
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// Ref is a parsed model reference.
type Ref struct {
	Input string
	// Name is the name the runner stores the model under, without the tag:
	// "ai/smollm2", "registry.example.com/team/model", or
	// "huggingface.co/bartowski/Llama-3.2-1B-Instruct-GGUF".
	Name string
	Tag  string // "latest", "v1"; for Hugging Face a quantization such as "Q4_K_M"

	HF   bool
	Repo string // Hugging Face repository, e.g. "bartowski/Llama-3.2-1B-Instruct-GGUF"

	OCI name.Reference // registry reference for non-Hugging Face models
}

// String returns "name:tag", the runner's key for the model.
func (r Ref) String() string { return r.Name + ":" + r.Tag }

var hfPrefixes = []string{"hf.co/", "huggingface.co/"}

// Parse mirrors the runner's normalization:
//
//	gemma3                     -> ai/gemma3:latest (default org ai/, name lowercased)
//	myorg/model:v2             -> myorg/model:v2
//	hf.co/org/Model-GGUF       -> huggingface.co/org/Model-GGUF:latest (case kept)
//	hf.co/org/Model-GGUF:Q8_0  -> huggingface.co/org/Model-GGUF:Q8_0
func Parse(s string) (Ref, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return Ref{}, fmt.Errorf("empty model reference")
	}
	n, hf := in, false
	for _, p := range hfPrefixes {
		if strings.HasPrefix(strings.ToLower(n), p) {
			n, hf = "huggingface.co/"+n[len(p):], true
			break
		}
	}
	if !hf && strings.Contains(n, "@") {
		ref, err := name.ParseReference(n)
		if err != nil {
			return Ref{}, fmt.Errorf("invalid model reference %q: %w", s, err)
		}
		base, dgst, _ := strings.Cut(n, "@")
		return Ref{Input: in, Name: base, Tag: dgst, OCI: ref}, nil
	}

	tag := "latest"
	if i := strings.LastIndex(n, ":"); i > strings.LastIndex(n, "/") {
		if t := n[i+1:]; t != "" {
			tag = t
		}
		n = n[:i]
	}
	if hf {
		repo := strings.TrimPrefix(n, "huggingface.co/")
		if repo == "" {
			return Ref{}, fmt.Errorf("invalid Hugging Face reference %q", s)
		}
		return Ref{Input: in, Name: n, Tag: tag, HF: true, Repo: repo}, nil
	}
	if !strings.Contains(n, "/") {
		n = "ai/" + n
	}
	n = strings.ToLower(n)
	ref, err := name.ParseReference(n + ":" + tag)
	if err != nil {
		return Ref{}, fmt.Errorf("invalid model reference %q: %w", s, err)
	}
	return Ref{Input: in, Name: n, Tag: tag, OCI: ref}, nil
}
