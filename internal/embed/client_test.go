package embed_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/williamsmiths/kongcowork-harness/internal/embed"
	"github.com/williamsmiths/kongcowork-harness/internal/embed/embedtest"
)

func TestClientEmbed(t *testing.T) {
	ctx := context.Background()
	t.Run("batches of at most 32, order kept, model checked once", func(t *testing.T) {
		fake := embedtest.New(t)
		c := fake.Client()
		texts := make([]string, 70)
		for i := range texts {
			texts[i] = fmt.Sprintf("mục số %d", i)
		}
		vecs, err := c.Embed(ctx, texts)
		if err != nil || len(vecs) != 70 || fake.Requests() != 3 {
			t.Fatalf("70 texts: %d vectors, %d requests, %v", len(vecs), fake.Requests(), err)
		}
		for i, v := range vecs {
			if len(v) != embed.DefaultDim || !slices.Equal(v, fake.Vector(texts[i])) {
				t.Fatalf("vector %d is not the vector of its text", i)
			}
		}
		if _, err := c.Embed(ctx, texts[:1]); err != nil || fake.Infos() != 1 {
			t.Fatalf("/info asked %d times, want once: %v", fake.Infos(), err)
		}
		if v, err := c.Embed(ctx, nil); v != nil || err != nil {
			t.Fatalf("no texts: %v %v", v, err)
		}
	})

	t.Run("refuses a sidecar that serves another model", func(t *testing.T) {
		fake := embedtest.New(t)
		fake.SetModel("intfloat/multilingual-e5-base")
		c := fake.Client()
		if _, err := c.Embed(ctx, []string{"a"}); !errors.Is(err, embed.ErrModelMismatch) || fake.Requests() != 0 {
			t.Fatalf("mismatch at /info: %v, %d embedding requests", err, fake.Requests())
		}
		fake.SetModel(embed.DefaultModel) // a mismatch is not cached: the right model recovers
		if _, err := c.Embed(ctx, []string{"a"}); err != nil {
			t.Fatalf("after the model is right again: %v", err)
		}
		fake.SetModel("another/model") // swapped after the check: the answer names the model that ran
		if _, err := c.Embed(ctx, []string{"b"}); !errors.Is(err, embed.ErrModelMismatch) {
			t.Fatalf("mismatch in the answer: %v", err)
		}
	})

	t.Run("wrong dimension", func(t *testing.T) {
		fake := embedtest.New(t)
		c := fake.Client()
		c.Dim = 768
		if _, err := c.Embed(ctx, []string{"a"}); !errors.Is(err, embed.ErrBadResponse) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("AC-S2-023 typed errors: unavailable, timeout, rejected", func(t *testing.T) {
		fake := embedtest.New(t)
		c := fake.Client()
		fake.SetStatus(503)
		if _, err := c.Embed(ctx, []string{"a"}); !errors.Is(err, embed.ErrUnavailable) {
			t.Fatalf("503: %v", err)
		}
		fake.SetStatus(0)
		if _, err := c.Embed(ctx, []string{"x " + embedtest.RejectMarker}); !errors.Is(err, embed.ErrRejected) {
			t.Fatalf("400: %v", err)
		}

		fake.SetDelay(5 * time.Second)
		c.Timeout = 100 * time.Millisecond
		start := time.Now()
		_, err := c.Embed(ctx, []string{"a"})
		if !errors.Is(err, embed.ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
			t.Fatalf("timeout: %v after %s", err, time.Since(start))
		}

		closed := httptest.NewServer(nil)
		closed.Close()
		down := &embed.Client{BaseURL: closed.URL, Model: embed.DefaultModel, Dim: embed.DefaultDim}
		if _, err := down.Embed(ctx, []string{"a"}); !errors.Is(err, embed.ErrUnavailable) {
			t.Fatalf("connection refused: %v", err)
		}
		if _, err := (&embed.Client{}).Embed(ctx, []string{"a"}); !errors.Is(err, embed.ErrUnavailable) {
			t.Fatalf("not configured: %v", err)
		}
	})
}

func TestQueryCache(t *testing.T) {
	ctx := context.Background()
	fake := embedtest.New(t)
	c := fake.Client()
	c.CacheSize = 2
	embedQ := func(text string) {
		t.Helper()
		v, err := c.EmbedQuery(ctx, text)
		if err != nil || !slices.Equal(v, fake.Vector(text)) {
			t.Fatalf("EmbedQuery(%q): %v", text, err)
		}
	}
	embedQ("a")
	embedQ("a")
	if fake.Requests() != 1 {
		t.Fatalf("same query twice reached the sidecar %d times", fake.Requests())
	}
	embedQ("b")
	embedQ("a") // a is now the most recent
	embedQ("c") // evicts b
	if c.CachedQueries() != 2 || fake.Requests() != 3 {
		t.Fatalf("cache holds %d entries after %d requests, want 2 and 3", c.CachedQueries(), fake.Requests())
	}
	embedQ("a")
	embedQ("b")
	if fake.Requests() != 4 {
		t.Fatalf("after eviction: %d requests, want 4 (a cached, b embedded again)", fake.Requests())
	}

	fake.SetStatus(503) // errors are not cached
	if _, err := c.EmbedQuery(ctx, "d"); !errors.Is(err, embed.ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	fake.SetStatus(0)
	embedQ("d")
}

func TestFromEnv(t *testing.T) {
	for _, k := range []string{"HARNESS_EMBED_URL", "HARNESS_EMBED_MODEL", "HARNESS_EMBED_DIM", "HARNESS_EMBED_MIN_SIM"} {
		t.Setenv(k, "")
	}
	cfg, err := embed.FromEnv()
	if err != nil || cfg.Client() != nil || cfg.Model != "BAAI/bge-m3" || cfg.Dim != 1024 || cfg.MinSim != 0.51 {
		t.Fatalf("defaults (off): %+v %v", cfg, err)
	}
	t.Setenv("HARNESS_EMBED_URL", "http://127.0.0.1:8870")
	cfg, err = embed.FromEnv()
	if c := cfg.Client(); err != nil || c == nil || c.BaseURL != "http://127.0.0.1:8870" || c.Model != "BAAI/bge-m3" || c.Dim != 1024 {
		t.Fatalf("on with defaults: %+v %v", cfg, err)
	}
	t.Setenv("HARNESS_EMBED_MODEL", "intfloat/multilingual-e5-base")
	t.Setenv("HARNESS_EMBED_DIM", "768")
	t.Setenv("HARNESS_EMBED_MIN_SIM", "0.78")
	if cfg, err = embed.FromEnv(); err != nil || cfg.Model != "intfloat/multilingual-e5-base" || cfg.Dim != 768 || cfg.MinSim != 0.78 {
		t.Fatalf("overrides: %+v %v", cfg, err)
	}
	for name, env := range map[string][2]string{
		"off-machine host": {"HARNESS_EMBED_URL", "http://embeddings.example.com"},
		"not a url":        {"HARNESS_EMBED_URL", "127.0.0.1:8870"},
		"model":            {"HARNESS_EMBED_MODEL", "x'; DROP TABLE knowledge; --"},
		"dim":              {"HARNESS_EMBED_DIM", "many"},
		"threshold":        {"HARNESS_EMBED_MIN_SIM", "1.5"},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			t.Setenv(env[0], env[1])
			if _, err := embed.FromEnv(); err == nil {
				t.Fatalf("%s=%q accepted", env[0], env[1])
			}
		})
	}
}

func TestLiteralAndClip(t *testing.T) {
	if got := embed.Literal([]float32{0.5, -1, 1e-7}); got != "[0.5,-1,1e-07]" {
		t.Fatalf("Literal: %s", got)
	}
	long := strings.Repeat("ộ", embed.MaxTextRunes+10)
	if got := embed.Clip(long); len([]rune(got)) != embed.MaxTextRunes {
		t.Fatalf("Clip kept %d characters", len([]rune(got)))
	}
	if embed.ItemText("tiêu đề", "nội dung") != "tiêu đề\nnội dung" {
		t.Fatal("ItemText is not title + newline + body")
	}
}
