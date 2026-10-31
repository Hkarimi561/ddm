package modelref

import "testing"

// Cases follow the runner's own normalize_test.go.
func TestParse(t *testing.T) {
	tests := []struct {
		in, want string
		hf       bool
		repo     string
	}{
		{"gemma3", "ai/gemma3:latest", false, ""},
		{"gemma3:v1", "ai/gemma3:v1", false, ""},
		{"  gemma3  ", "ai/gemma3:latest", false, ""},
		{"model:", "ai/model:latest", false, ""},
		{"MyModel", "ai/mymodel:latest", false, ""},
		{"ai/smollm2", "ai/smollm2:latest", false, ""},
		{"myorg/model:v2", "myorg/model:v2", false, ""},
		{"registry.example.com/myorg/model:v1", "registry.example.com/myorg/model:v1", false, ""},
		{"localhost:5000/model", "localhost:5000/model:latest", false, ""},
		{"hf.co/org/model", "huggingface.co/org/model:latest", true, "org/model"},
		{"hf.co/bartowski/Llama-3.2-1B-Instruct-GGUF:Q4_K_M", "huggingface.co/bartowski/Llama-3.2-1B-Instruct-GGUF:Q4_K_M", true, "bartowski/Llama-3.2-1B-Instruct-GGUF"},
		{"huggingface.co/HuggingFaceTB/SmolLM2-135M-Instruct", "huggingface.co/HuggingFaceTB/SmolLM2-135M-Instruct:latest", true, "HuggingFaceTB/SmolLM2-135M-Instruct"},
		{"HF.co/org/model:Q8_0", "huggingface.co/org/model:Q8_0", true, "org/model"},
	}
	for _, tt := range tests {
		r, err := Parse(tt.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.in, err)
			continue
		}
		if r.String() != tt.want || r.HF != tt.hf || r.Repo != tt.repo {
			t.Errorf("Parse(%q) = %q hf=%v repo=%q; want %q hf=%v repo=%q", tt.in, r.String(), r.HF, r.Repo, tt.want, tt.hf, tt.repo)
		}
		if !r.HF && r.OCI == nil {
			t.Errorf("Parse(%q): no registry reference", tt.in)
		}
	}
	for _, bad := range []string{"", "   ", "hf.co/"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	r, _ := Parse("ai/smollm2")
	if got := r.OCI.Context().RepositoryStr(); got != "ai/smollm2" {
		t.Errorf("Docker Hub repository = %s, want ai/smollm2", got)
	}
}
