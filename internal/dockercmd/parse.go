// Package dockercmd works out which images a docker command line needs,
// without running it. Anything it doesn't understand is left alone, so the
// worst case is that docker downloads something itself, as it always would.
package dockercmd

import "strings"

// Kind is the kind of command ddm recognized.
type Kind int

const (
	Other       Kind = iota // needs no downloads (or isn't understood): run docker as is
	Run                     // docker run / create: make sure the image exists, then run docker
	Pull                    // docker pull: ddm does the pull itself
	ComposeUp               // docker compose up / create / run: make sure service images exist, then run docker
	ComposePull             // docker compose pull: ddm pulls the service images itself
	ModelRun                // docker model run: make sure the model is loaded, then run docker
	ModelPull               // docker model pull: ddm pulls the model itself
)

// Image is an image a command needs.
type Image struct {
	Ref      string
	Platform string // empty: the daemon's platform
}

// Command is what ddm understood of a docker command line.
type Command struct {
	// Global holds docker's own options (--context, -H, ...). ddm passes
	// them to every docker command it runs, so it talks to the same engine.
	Global  []string
	Kind    Kind
	Images  []Image  // for Run and Pull
	Compose *Compose // for ComposeUp and ComposePull
	Model   string   // for ModelRun and ModelPull
}

// Compose describes a docker compose command.
type Compose struct {
	// Options are compose's options before the subcommand (-f, --profile,
	// -p, --env-file, ...), reused for `docker compose config`.
	Options  []string
	Services []string // services named on the command line; empty means all
	NoDeps   bool     // --no-deps: dependencies are not started
}

// Parse analyzes the arguments that follow "docker".
func Parse(args []string) Command {
	var c Command
	i := 0
	for i < len(args) && isFlag(args[i]) {
		name, _, hasVal := strings.Cut(args[i], "=")
		c.Global = append(c.Global, args[i])
		i++
		if !hasVal && globalSpec.takesValue(name) && i < len(args) {
			c.Global = append(c.Global, args[i])
			i++
		}
	}
	if i >= len(args) {
		return c
	}
	sub, rest := args[i], args[i+1:]
	if (sub == "container" || sub == "image") && len(rest) > 0 {
		sub, rest = sub+" "+rest[0], rest[1:]
	}
	switch sub {
	case "run", "create", "container run", "container create":
		if img, ok := parseRun(rest); ok {
			c.Kind, c.Images = Run, []Image{img}
		}
	case "pull", "image pull":
		if img, ok := parsePull(rest); ok {
			c.Kind, c.Images = Pull, []Image{img}
		}
	case "compose":
		c.Kind, c.Compose = parseCompose(rest)
	case "model":
		if len(rest) == 0 {
			break
		}
		kind := map[string]Kind{"run": ModelRun, "pull": ModelPull}[rest[0]]
		if kind == Other {
			break
		}
		if pos := modelSpec.scan(rest[1:], true, func(string, string) {}); len(pos) > 0 {
			c.Kind, c.Model = kind, pos[0]
		}
	}
	return c
}

func isFlag(a string) bool { return strings.HasPrefix(a, "-") && a != "-" }

// flagSpec says which options of a command are switches (take no value).
type flagSpec struct {
	isSwitch func(name string) bool // name as "--rm" or "-d"
}

func (s flagSpec) takesValue(name string) bool { return !s.isSwitch(name) }

// switches lists the switches; every other option takes a value. This suits
// commands where most options take values, like docker run.
func switches(long []string, short string) flagSpec {
	m := set(long...)
	return flagSpec{isSwitch: func(name string) bool {
		if strings.HasPrefix(name, "--") {
			return m[name]
		}
		return len(name) == 2 && strings.IndexByte(short, name[1]) >= 0
	}}
}

// values lists the options that take a value; every other option is a
// switch. This suits commands where most options are switches.
func values(names ...string) flagSpec {
	m := set(names...)
	return flagSpec{isSwitch: func(name string) bool { return !m[name] }}
}

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[s] = true
	}
	return m
}

// scan walks args, calling onFlag for each option with its value (if it
// takes one), and returns the positional arguments. With firstOnly, it stops
// at the first positional argument: after `docker run ... IMAGE` everything
// belongs to the container's command.
func (s flagSpec) scan(args []string, firstOnly bool, onFlag func(name, value string)) []string {
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !isFlag(a) {
			pos = append(pos, a)
			if firstOnly {
				break
			}
			continue
		}
		if strings.HasPrefix(a, "--") {
			name, val, hasVal := strings.Cut(a, "=")
			if !hasVal && s.takesValue(name) && i+1 < len(args) {
				i++
				val = args[i]
			}
			onFlag(name, val)
			continue
		}
		// A group of short options like -it or -dp 80:80: the first option
		// that takes a value consumes the rest of the group, or the next
		// argument when it comes last.
		letters := a[1:]
		for j := 0; j < len(letters); j++ {
			name := "-" + letters[j:j+1]
			if s.isSwitch(name) {
				onFlag(name, "")
				continue
			}
			val := letters[j+1:]
			if val == "" && i+1 < len(args) {
				i++
				val = args[i]
			}
			onFlag(name, val)
			break
		}
	}
	return pos
}

var (
	globalSpec = values("--config", "-c", "--context", "-H", "--host", "-l", "--log-level",
		"--tlscacert", "--tlscert", "--tlskey")

	runSpec = switches([]string{"--detach", "--interactive", "--tty", "--rm", "--privileged",
		"--publish-all", "--read-only", "--init", "--no-healthcheck", "--oom-kill-disable",
		"--sig-proxy", "--help", "--quiet", "--disable-content-trust", "--use-api-socket"}, "ditPq")

	pullSpec = switches([]string{"--all-tags", "--disable-content-trust", "--quiet", "--help"}, "aq")

	composeSpec = values("-f", "--file", "-p", "--project-name", "--profile", "--project-directory",
		"--env-file", "--ansi", "--progress", "--parallel")

	composeUpSpec = values("--attach", "--exit-code-from", "--no-attach", "--pull", "--scale",
		"-t", "--timeout", "--wait-timeout")

	composeRunSpec = values("--cap-add", "--cap-drop", "-e", "--env", "--env-from-file",
		"--entrypoint", "-l", "--label", "--name", "-p", "--publish", "--pull", "-u", "--user",
		"-v", "--volume", "-w", "--workdir")

	composePullSpec = values("--policy")

	// docker model run/pull: switches; any other option takes a value.
	modelSpec = switches([]string{"--detach", "--debug", "--ignore-runtime-memory-check", "--help"}, "d")
)

// parseRun finds the image of `docker run|create [OPTIONS] IMAGE [COMMAND]`.
func parseRun(args []string) (Image, bool) {
	var img Image
	pull := ""
	pos := runSpec.scan(args, true, func(name, value string) {
		switch name {
		case "--platform":
			img.Platform = value
		case "--pull":
			pull = value
		}
	})
	if len(pos) == 0 || pull == "never" {
		return Image{}, false
	}
	img.Ref = pos[0]
	return img, true
}

// parsePull understands `docker pull [OPTIONS] NAME`. Pulling every tag is
// left to docker.
func parsePull(args []string) (Image, bool) {
	var img Image
	allTags := false
	pos := pullSpec.scan(args, false, func(name, value string) {
		switch name {
		case "--platform":
			img.Platform = value
		case "-a", "--all-tags":
			allTags = true
		}
	})
	if len(pos) != 1 || allTags {
		return Image{}, false
	}
	img.Ref = pos[0]
	return img, true
}

// parseCompose understands `docker compose [OPTIONS] up|create|run|pull ...`.
func parseCompose(args []string) (Kind, *Compose) {
	c := &Compose{}
	i := 0
	for i < len(args) && isFlag(args[i]) {
		name, _, hasVal := strings.Cut(args[i], "=")
		c.Options = append(c.Options, args[i])
		i++
		if !hasVal && composeSpec.takesValue(name) && i < len(args) {
			c.Options = append(c.Options, args[i])
			i++
		}
	}
	if i >= len(args) {
		return Other, nil
	}
	sub, rest := args[i], args[i+1:]

	kind, spec := ComposeUp, composeUpSpec
	switch sub {
	case "up", "create":
	case "run":
		spec = composeRunSpec
	case "pull":
		kind, spec = ComposePull, composePullSpec
	default:
		return Other, nil
	}
	pullNever := false
	pos := spec.scan(rest, sub == "run", func(name, value string) {
		switch name {
		case "--no-deps":
			c.NoDeps = true
		case "--pull":
			pullNever = value == "never"
		}
	})
	if pullNever || (sub == "run" && len(pos) == 0) {
		return Other, nil
	}
	c.Services = pos
	return kind, c
}
