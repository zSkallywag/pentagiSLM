package ollama

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"pentagi/pkg/providers/pconfig"

	"github.com/ollama/ollama/api"
)

const (
	windowRefreshInterval = 15 * time.Second
	windowCallTimeout     = 3 * time.Second
)

// windowResolver learns the context window each model is actually served with.
// A local model runs with the num_ctx of its Modelfile or the daemon default,
// not its trained maximum, and PentAGI never sends num_ctx, so only the daemon
// knows the real ceiling. Without it the window-dependent guards (compaction,
// the answer clamp) stay off for every Ollama model.
type windowResolver struct {
	client *api.Client
	wanted []string
	now    func() time.Time

	mx       sync.Mutex
	polled   time.Time
	running  map[string]int
	declared map[string]int
	shown    map[string]bool
}

func newWindowResolver(client *api.Client, wanted []string) *windowResolver {
	return &windowResolver{
		client:   client,
		wanted:   wanted,
		now:      time.Now,
		running:  make(map[string]int),
		declared: make(map[string]int),
		shown:    make(map[string]bool),
	}
}

// apply returns models with ContextWindow filled for every model whose window
// the daemon reports. The context a model is loaded with wins over its declared
// one, and stays known after the model is unloaded: the daemon loads it at the
// same size next time. A model with no reported window keeps a nil window.
func (r *windowResolver) apply(models pconfig.ModelsConfig) pconfig.ModelsConfig {
	r.mx.Lock()
	defer r.mx.Unlock()

	r.refresh()

	out := make(pconfig.ModelsConfig, len(models))
	copy(out, models)
	for i := range out {
		if out[i].ContextWindow != nil {
			continue
		}
		name := canonicalModelName(out[i].Name)
		window, ok := r.running[name]
		if !ok {
			window, ok = r.declared[name]
		}
		if ok && window > 0 {
			out[i].ContextWindow = &window
		}
	}
	return out
}

func (r *windowResolver) refresh() {
	for _, name := range r.wanted {
		name = canonicalModelName(name)
		if r.shown[name] {
			continue
		}
		if window, ok := r.show(name); ok {
			r.declared[name] = window
		}
		r.shown[name] = true
	}

	if now := r.now(); r.polled.IsZero() || now.Sub(r.polled) >= windowRefreshInterval {
		r.polled = now
		r.poll()
	}
}

func (r *windowResolver) poll() {
	ctx, cancel := context.WithTimeout(context.Background(), windowCallTimeout)
	defer cancel()

	response, err := r.client.ListRunning(ctx)
	if err != nil {
		return
	}
	for _, model := range response.Models {
		if model.ContextLength > 0 {
			r.running[canonicalModelName(model.Name)] = model.ContextLength
		}
	}
}

// show reads the window a model declares: num_ctx from its Modelfile, or the
// trained context length of a cloud model, which the cloud serves in full. The
// trained length of a local model is not used, because the daemon does not load
// it at that size unless told to.
func (r *windowResolver) show(name string) (int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), windowCallTimeout)
	defer cancel()

	response, err := r.client.Show(ctx, &api.ShowRequest{Model: name})
	if err != nil {
		return 0, false
	}
	if window, ok := modelfileNumCtx(response.Parameters); ok {
		return window, true
	}
	if response.RemoteHost != "" || response.RemoteModel != "" {
		return trainedContextLength(response.ModelInfo)
	}
	return 0, false
}

func modelfileNumCtx(parameters string) (int, bool) {
	for line := range strings.SplitSeq(parameters, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "num_ctx" {
			continue
		}
		if window, err := strconv.Atoi(fields[1]); err == nil && window > 0 {
			return window, true
		}
	}
	return 0, false
}

func trainedContextLength(info map[string]any) (int, bool) {
	arch, _ := info["general.architecture"].(string)
	if arch == "" {
		return 0, false
	}
	if length, ok := info[arch+".context_length"].(float64); ok && length > 0 {
		return int(length), true
	}
	return 0, false
}

// canonicalModelName adds the tag the daemon reports for an untagged name, so a
// configured "qwen3" matches a running "qwen3:latest".
func canonicalModelName(name string) string {
	if name == "" || strings.Contains(name[strings.LastIndex(name, "/")+1:], ":") {
		return name
	}
	return name + ":latest"
}
