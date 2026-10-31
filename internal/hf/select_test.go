package hf

import (
	"reflect"
	"strings"
	"testing"
)

func files(paths ...string) []File {
	var out []File
	for _, p := range paths {
		out = append(out, File{Path: p, Size: 100, SHA256: strings.Repeat("a", 64)})
	}
	return out
}

func paths(fs []File) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Path)
	}
	return out
}

func TestSelectGGUF(t *testing.T) {
	repo := files(
		"README.md", ".gitattributes",
		"Llama-3.2-1B-Instruct-IQ4_XS.gguf",
		"Llama-3.2-1B-Instruct-Q4_K_M.gguf",
		"Llama-3.2-1B-Instruct-Q4_K_M_L.gguf",
		"Llama-3.2-1B-Instruct-Q8_0.gguf",
		"Llama-3.2-1B-Instruct-f16.gguf",
	)
	tests := []struct {
		tag  string
		want []string
	}{
		{"", []string{"Llama-3.2-1B-Instruct-Q4_K_M.gguf"}},
		{"latest", []string{"Llama-3.2-1B-Instruct-Q4_K_M.gguf"}},
		{"Q8_0", []string{"Llama-3.2-1B-Instruct-Q8_0.gguf"}},
		{"q8_0", []string{"Llama-3.2-1B-Instruct-Q8_0.gguf"}},
		{"IQ4_XS", []string{"Llama-3.2-1B-Instruct-IQ4_XS.gguf"}},
		{"F16", []string{"Llama-3.2-1B-Instruct-f16.gguf"}},
	}
	for _, tt := range tests {
		p, err := Select(repo, tt.tag)
		if err != nil {
			t.Errorf("tag %q: %v", tt.tag, err)
			continue
		}
		if p.Format != GGUF || !reflect.DeepEqual(paths(p.Weights), tt.want) {
			t.Errorf("tag %q: %v, want %v", tt.tag, paths(p.Weights), tt.want)
		}
	}
	if _, err := Select(repo, "Q2_K"); err == nil {
		t.Error("a missing quantization was accepted")
	}
	if _, err := Select(repo, "Q4_XS"); err == nil {
		t.Error("Q4_XS matched part of IQ4_XS")
	}
}

func TestSelectShardsAndDefaultFallback(t *testing.T) {
	repo := files(
		"Q8_0/big-Q8_0-00002-of-00002.gguf",
		"Q8_0/big-Q8_0-00001-of-00002.gguf",
		"Q6_K/big-Q6_K-00001-of-00003.gguf",
		"Q6_K/big-Q6_K-00002-of-00003.gguf",
		"Q6_K/big-Q6_K-00003-of-00003.gguf",
	)
	p, err := Select(repo, "Q8_0")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Q8_0/big-Q8_0-00001-of-00002.gguf", "Q8_0/big-Q8_0-00002-of-00002.gguf"}; !reflect.DeepEqual(paths(p.Weights), want) {
		t.Errorf("Q8_0 shards: %v", paths(p.Weights))
	}
	// No Q4_K_M: the default falls back to the first GGUF file and its shards.
	p, err = Select(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Q6_K/big-Q6_K-00001-of-00003.gguf", "Q6_K/big-Q6_K-00002-of-00003.gguf", "Q6_K/big-Q6_K-00003-of-00003.gguf"}; !reflect.DeepEqual(paths(p.Weights), want) {
		t.Errorf("default fallback: %v", paths(p.Weights))
	}
}

func TestSelectSingleFileIgnoresTag(t *testing.T) {
	p, err := Select(files("model.gguf"), "Q8_0")
	if err != nil || !reflect.DeepEqual(paths(p.Weights), []string{"model.gguf"}) {
		t.Fatalf("%v %v", p, err)
	}
}

func TestSelectProjectorAndTemplate(t *testing.T) {
	p, err := Select(files("gemma-Q4_K_M.gguf", "gemma-Q8_0.gguf", "mmproj-gemma-f32.gguf", "mmproj-gemma-F16.gguf", "chat_template.jinja", "config.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths(p.Weights), []string{"gemma-Q4_K_M.gguf"}) {
		t.Errorf("weights %v (the projector must not count as a weight)", paths(p.Weights))
	}
	if p.MMProj == nil || p.MMProj.Path != "mmproj-gemma-F16.gguf" {
		t.Errorf("projector %+v, want the F16 one", p.MMProj)
	}
	if p.ChatTemplate == nil || p.ChatTemplate.Path != "chat_template.jinja" {
		t.Errorf("chat template %+v", p.ChatTemplate)
	}
	if got := len(p.Files()); got != 3 {
		t.Errorf("files to download: %v", paths(p.Files()))
	}
}

func TestSelectSafetensors(t *testing.T) {
	p, err := Select(files("model-00002-of-00002.safetensors", "model-00001-of-00002.safetensors", "config.json",
		"tokenizer.json", "tokenizer.model", "generation_config.json", "README.md", "chat_template.jinja"), "latest")
	if err != nil {
		t.Fatal(err)
	}
	if p.Format != Safetensors || len(p.Weights) != 2 || p.Weights[0].Path != "model-00001-of-00002.safetensors" {
		t.Errorf("weights %v", paths(p.Weights))
	}
	if want := []string{"chat_template.jinja", "config.json", "generation_config.json", "tokenizer.json", "tokenizer.model"}; !reflect.DeepEqual(paths(p.Extra), want) {
		t.Errorf("extra files %v, want %v", paths(p.Extra), want)
	}
	if p.WeightBytes() != 200 {
		t.Errorf("weight bytes %d", p.WeightBytes())
	}
}

func TestSelectNothing(t *testing.T) {
	if _, err := Select(files("README.md", "model.bin"), ""); err == nil {
		t.Error("a repo without GGUF or safetensors was accepted")
	}
}
