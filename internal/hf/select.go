package hf

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// DefaultQuantization is what Docker Model Runner picks for a GGUF repo
// when the reference has no tag (or "latest"/"main").
const DefaultQuantization = "Q4_K_M"

// Format of a model repository.
type Format string

const (
	GGUF        Format = "gguf"
	Safetensors Format = "safetensors"
)

// Plan lists the files to download for a model and how to package them.
type Plan struct {
	Format       Format
	Weights      []File // GGUF: the selected (possibly sharded) files, sorted; safetensors: all of them
	MMProj       *File  // multimodal projector (GGUF)
	ChatTemplate *File  // Jinja chat template (GGUF)
	Extra        []File // configs and tokenizer files (safetensors)
}

// Files returns every file in the plan.
func (p *Plan) Files() []File {
	out := append([]File(nil), p.Weights...)
	if p.MMProj != nil {
		out = append(out, *p.MMProj)
	}
	if p.ChatTemplate != nil {
		out = append(out, *p.ChatTemplate)
	}
	return append(out, p.Extra...)
}

// WeightBytes is the size of the weights, what has to fit in memory.
func (p *Plan) WeightBytes() int64 {
	var n int64
	for _, f := range p.Weights {
		n += f.Size
	}
	return n
}

// Select decides what to download for a repository and a reference tag,
// following Docker Model Runner's rules:
//
//   - GGUF repos: the files of the quantization named by tag. No tag (or
//     "latest"/"main") means Q4_K_M, falling back to the first GGUF file.
//     All shards of a split file are included, plus the F16 multimodal
//     projector if there is one.
//   - Safetensors repos: every weight file plus configs and tokenizer files.
func Select(files []File, tag string) (*Plan, error) {
	var ggufs, projs, safet, jinja, extra []File
	for _, f := range files {
		lower := strings.ToLower(f.Path)
		base := path.Base(lower)
		switch {
		case strings.HasSuffix(lower, ".gguf") && strings.Contains(base, "mmproj"):
			projs = append(projs, f)
		case strings.HasSuffix(lower, ".gguf"):
			ggufs = append(ggufs, f)
		case strings.HasSuffix(lower, ".safetensors"):
			safet = append(safet, f)
		case strings.HasSuffix(lower, ".jinja"):
			jinja = append(jinja, f)
			extra = append(extra, f)
		case isConfigFile(base):
			extra = append(extra, f)
		}
	}
	byPath := func(fs []File) {
		sort.Slice(fs, func(i, j int) bool { return fs[i].Path < fs[j].Path })
	}
	byPath(ggufs)
	byPath(projs)
	byPath(jinja)

	if len(ggufs) > 0 {
		weights, err := selectGGUF(ggufs, tag)
		if err != nil {
			return nil, err
		}
		p := &Plan{Format: GGUF, Weights: weights}
		if len(projs) > 0 {
			p.MMProj = &projs[0]
			for i := range projs {
				if strings.Contains(strings.ToLower(path.Base(projs[i].Path)), "f16") {
					p.MMProj = &projs[i]
					break
				}
			}
		}
		if len(jinja) > 0 {
			p.ChatTemplate = &jinja[0]
		}
		return p, nil
	}
	if len(safet) > 0 {
		byPath(safet)
		byPath(extra)
		return &Plan{Format: Safetensors, Weights: safet, Extra: extra}, nil
	}
	return nil, fmt.Errorf("the repository has no GGUF or safetensors model files")
}

func isConfigFile(base string) bool {
	switch {
	case strings.HasSuffix(base, ".json"), strings.HasSuffix(base, ".txt") && base != "readme.txt",
		base == "tokenizer.model", strings.HasSuffix(base, ".tiktoken"):
		return true
	}
	return false
}

var shardRe = regexp.MustCompile(`(?i)^(.*)-\d{5}-of-\d{5}\.gguf$`)

func selectGGUF(ggufs []File, tag string) ([]File, error) {
	if len(ggufs) == 1 {
		return ggufs, nil
	}
	want, isDefault := tag, false
	if tag == "" || tag == "latest" || tag == "main" {
		want, isDefault = DefaultQuantization, true
	}
	re := quantPattern(want)
	var matched []File
	for _, f := range ggufs {
		if re.MatchString(f.Path) {
			matched = append(matched, f)
		}
	}
	if len(matched) > 0 {
		return matched, nil
	}
	if isDefault {
		return shardsOf(ggufs, ggufs[0]), nil
	}
	var names []string
	for _, f := range ggufs {
		names = append(names, path.Base(f.Path))
	}
	if len(names) > 8 {
		names = append(names[:8], "…")
	}
	return nil, fmt.Errorf("no GGUF file for quantization %q (files: %s)", tag, strings.Join(names, ", "))
}

// quantPattern matches a quantization name as a whole token in a path, so
// Q4_K_M doesn't match Q4_K_M_L and Q4_XS doesn't match IQ4_XS.
func quantPattern(q string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^a-z0-9])` + regexp.QuoteMeta(q) + `($|[^a-z0-9_])`)
}

// shardsOf returns f together with the other shards of the same split file.
func shardsOf(all []File, f File) []File {
	m := shardRe.FindStringSubmatch(f.Path)
	if m == nil {
		return []File{f}
	}
	var out []File
	for _, g := range all {
		if gm := shardRe.FindStringSubmatch(g.Path); gm != nil && gm[1] == m[1] {
			out = append(out, g)
		}
	}
	return out
}
