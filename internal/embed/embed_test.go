package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorizond/vcs-rag/internal/config"
)

// TestOllamaEmbedBatching: батчи по batch, форма /api/embed, валидация размерности.
func TestOllamaEmbedBatching(t *testing.T) {
	var requests atomic.Int32
	var badPath, badModel, badKeepAlive atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			badPath.Store(true)
		}
		requests.Add(1)
		var req struct {
			Model     string   `json:"model"`
			Input     []string `json:"input"`
			KeepAlive string   `json:"keep_alive"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "bge-m3" {
			badModel.Store(true)
		}
		if req.KeepAlive != "10m" {
			badKeepAlive.Store(true)
		}
		vecs := make([][]float32, len(req.Input))
		for i := range req.Input {
			vecs[i] = []float32{float32(i), 0.5}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": vecs})
	}))
	defer srv.Close()

	p := NewOllama(srv.URL, "bge-m3", 2, 2) // dim=2, batch=2
	vecs, err := p.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 3 || len(vecs[0]) != 2 {
		t.Fatalf("векторов %d (dim %d), ожидалось 3×2", len(vecs), len(vecs[0]))
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("запросов %d, ожидалось 2 (батчи 2+1)", n)
	}
	if badPath.Load() || badModel.Load() || badKeepAlive.Load() {
		t.Errorf("форма запроса неверна: path=%v model=%v keep_alive=%v", badPath.Load(), badModel.Load(), badKeepAlive.Load())
	}
}

// TestLMStudioOrdering: ответ /v1/embeddings сортируется по index, есть Authorization.
func TestLMStudioOrdering(t *testing.T) {
	var badAuth, badPath atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			badPath.Store(true)
		}
		if r.Header.Get("Authorization") != "Bearer lm-studio" {
			badAuth.Store(true)
		}
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, 0, len(req.Input))
		for i := len(req.Input) - 1; i >= 0; i-- { // намеренно в обратном порядке
			data = append(data, map[string]any{"index": i, "embedding": []float32{float32(i), 1}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	p := NewLMStudio(srv.URL, "bge-m3", 2, 10)
	vecs, err := p.Embed(context.Background(), []string{"x", "y", "z"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("векторов %d, ожидалось 3", len(vecs))
	}
	for i, v := range vecs {
		if v[0] != float32(i) {
			t.Errorf("порядок нарушен: vecs[%d][0] = %v, ожидалось %d", i, v[0], i)
		}
	}
	if badPath.Load() || badAuth.Load() {
		t.Errorf("форма запроса неверна: path=%v auth=%v", badPath.Load(), badAuth.Load())
	}
}

// TestDimensionMismatch: несовпадение размерности — ошибка до записи (FR-009).
func TestDimensionMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{1, 2, 3}}})
	}))
	defer srv.Close()

	p := NewOllama(srv.URL, "bge-m3", 2, 2)
	_, err := p.Embed(context.Background(), []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "размерность") {
		t.Fatalf("err = %v, ожидалась ошибка о размерности", err)
	}
}

// TestFactory: выбор провайдера по конфигу.
func TestFactory(t *testing.T) {
	cfg := &config.Config{}
	cfg.Embeddings = config.Embeddings{Provider: "ollama", BaseURL: "http://x", Model: "m", Dim: 8, Batch: 4}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New(ollama): %v", err)
	}
	if _, ok := p.(*Ollama); !ok || p.Dim() != 8 {
		t.Errorf("New(ollama) = %T (dim %d)", p, p.Dim())
	}

	cfg.Embeddings.Provider = "lm-studio"
	p, err = New(cfg)
	if err != nil {
		t.Fatalf("New(lm-studio): %v", err)
	}
	if _, ok := p.(*LMStudio); !ok {
		t.Errorf("New(lm-studio) = %T", p)
	}

	cfg.Embeddings.Provider = "openai"
	if _, err := New(cfg); err == nil {
		t.Error("New(openai) = nil error, ожидалась ошибка")
	}
}
