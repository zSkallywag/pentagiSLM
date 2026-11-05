package ollama

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"pentagi/pkg/config"
	"pentagi/pkg/providers/pconfig"
	"pentagi/pkg/providers/provider"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeDaemon struct {
	mx      sync.Mutex
	running string
	shows   map[string]string
	psCalls int
	shown   int
}

func (d *fakeDaemon) setRunning(body string) {
	d.mx.Lock()
	defer d.mx.Unlock()
	d.running = body
}

func (d *fakeDaemon) calls() (int, int) {
	d.mx.Lock()
	defer d.mx.Unlock()
	return d.psCalls, d.shown
}

func (d *fakeDaemon) serve(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mx.Lock()
		defer d.mx.Unlock()
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/api/ps":
			d.psCalls++
			fmt.Fprint(w, d.running)
		case "/api/show":
			d.shown++
			raw, _ := io.ReadAll(r.Body)
			var req struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(raw, &req)
			body, ok := d.shows[req.Model]
			if !ok {
				http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
				return
			}
			fmt.Fprint(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	return srv
}

const contextWindowAgents = `
simple:
  model: deepseek-v4.1-flash:cloud
pentester:
  model: qwen3.5-9b
searcher:
  model: hf.co/HauhauCS/Qwen3.5-9B-GGUF:Q4_K_M
coder:
  model: llama3.1:8b
installer:
  model: mistral:7b
`

func contextWindowProvider(t *testing.T, daemon *fakeDaemon, smallModel bool) *ollamaProvider {
	t.Helper()

	srv := daemon.serve(t)
	cfg := &config.Config{
		OllamaServerURL:   srv.URL,
		OllamaServerModel: "mistral:7b",
		SmallModelMode:    smallModel,
	}
	providerConfig, err := BuildProviderConfig(cfg, []byte(contextWindowAgents))
	require.NoError(t, err)
	prov, err := New(cfg, provider.DefaultProviderNameOllama, providerConfig)
	require.NoError(t, err)

	return prov.(*ollamaProvider)
}

func contextWindows(models pconfig.ModelsConfig) map[string]*int {
	windows := make(map[string]*int, len(models))
	for _, model := range models {
		windows[model.Name] = model.ContextWindow
	}
	return windows
}

func TestContextWindow_Apply_ReportsTheWindowEachModelIsServedWith(t *testing.T) {
	daemon := &fakeDaemon{
		running: `{"models":[
			{"name":"qwen3.5-9b:latest","model":"qwen3.5-9b:latest","context_length":16384},
			{"name":"hf.co/HauhauCS/Qwen3.5-9B-GGUF:Q4_K_M","model":"hf.co/HauhauCS/Qwen3.5-9B-GGUF:Q4_K_M","context_length":12288}
		]}`,
		shows: map[string]string{
			"qwen3.5-9b:latest": `{"parameters":"temperature 0.6","model_info":{` +
				`"general.architecture":"qwen35","qwen35.context_length":262144}}`,
			"deepseek-v4.1-flash:cloud": `{"remote_host":"https://ollama.com:443","remote_model":"deepseek-v4.1-flash",` +
				`"model_info":{"general.architecture":"deepseek4","deepseek4.context_length":131072}}`,
			"llama3.1:8b": `{"parameters":"num_ctx                        8192\nstop                           \"<|eot_id|>\"",` +
				`"model_info":{"general.architecture":"llama","llama.context_length":131072}}`,
			"mistral:7b": `{"model_info":{"general.architecture":"llama","llama.context_length":32768}}`,
		},
	}
	windows := contextWindows(contextWindowProvider(t, daemon, true).GetModels())

	require.NotNil(t, windows["qwen3.5-9b"], "an untagged name matches the daemon's :latest")
	assert.Equal(t, 16384, *windows["qwen3.5-9b"], "the loaded context wins over the trained length")

	require.NotNil(t, windows["hf.co/HauhauCS/Qwen3.5-9B-GGUF:Q4_K_M"])
	assert.Equal(t, 12288, *windows["hf.co/HauhauCS/Qwen3.5-9B-GGUF:Q4_K_M"], "a registry path with a tag is matched as is")

	require.NotNil(t, windows["deepseek-v4.1-flash:cloud"])
	assert.Equal(t, 131072, *windows["deepseek-v4.1-flash:cloud"], "a cloud model is served at its trained length")

	require.NotNil(t, windows["llama3.1:8b"])
	assert.Equal(t, 8192, *windows["llama3.1:8b"], "an unloaded model reports its Modelfile num_ctx")

	assert.Nil(t, windows["mistral:7b"], "the trained length of an unloaded local model is not its window")
}

func TestContextWindow_Apply_KeepsTheLoadedWindowAfterTheModelUnloads(t *testing.T) {
	daemon := &fakeDaemon{
		running: `{"models":[{"name":"qwen3.5-9b:latest","context_length":16384}]}`,
		shows:   map[string]string{},
	}
	prov := contextWindowProvider(t, daemon, true)
	clock := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	prov.windows.now = func() time.Time { return clock }

	require.NotNil(t, contextWindows(prov.GetModels())["qwen3.5-9b"])

	daemon.setRunning(`{"models":[]}`)
	clock = clock.Add(time.Minute)
	window := contextWindows(prov.GetModels())["qwen3.5-9b"]
	require.NotNil(t, window)
	assert.Equal(t, 16384, *window)

	daemon.setRunning(`{"models":[{"name":"qwen3.5-9b:latest","context_length":32768}]}`)
	clock = clock.Add(time.Minute)
	window = contextWindows(prov.GetModels())["qwen3.5-9b"]
	require.NotNil(t, window)
	assert.Equal(t, 32768, *window, "a reload at another size replaces the remembered window")
}

func TestContextWindow_Apply_AsksTheDaemonAtMostOncePerInterval(t *testing.T) {
	daemon := &fakeDaemon{running: `{"models":[]}`, shows: map[string]string{}}
	prov := contextWindowProvider(t, daemon, true)
	clock := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	prov.windows.now = func() time.Time { return clock }

	prov.GetModels()
	_, shownFirst := daemon.calls()
	clock = clock.Add(5 * time.Second)
	prov.GetModels()
	prov.GetModels()

	psCalls, shown := daemon.calls()
	assert.Equal(t, 1, psCalls, "running models are polled once per interval")
	assert.Equal(t, shownFirst, shown, "each model is shown once, even when the daemon does not know it")

	clock = clock.Add(20 * time.Second)
	prov.GetModels()
	psCalls, _ = daemon.calls()
	assert.Equal(t, 2, psCalls)
}

func TestContextWindow_Apply_IsOffWithoutSmallModelMode(t *testing.T) {
	daemon := &fakeDaemon{
		running: `{"models":[{"name":"qwen3.5-9b:latest","context_length":16384}]}`,
		shows:   map[string]string{},
	}
	models := contextWindowProvider(t, daemon, false).GetModels()

	for _, model := range models {
		assert.Nil(t, model.ContextWindow, "%s must keep the unknown window it has upstream", model.Name)
	}
	psCalls, shown := daemon.calls()
	assert.Zero(t, psCalls)
	assert.Zero(t, shown)
}
