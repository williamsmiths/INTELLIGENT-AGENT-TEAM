package embed

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
)

// Config is the daemon's configuration surface of semantic retrieval.
type Config struct {
	URL    string  // HARNESS_EMBED_URL; empty = semantic retrieval off
	Model  string  // HARNESS_EMBED_MODEL
	Dim    int     // HARNESS_EMBED_DIM
	MinSim float64 // HARNESS_EMBED_MIN_SIM: minimum cosine similarity of a semantic hit
}

// modelRe bounds what may be used as a model name (it is also written into SQL as a literal).
var modelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

// ValidModel reports whether name is an acceptable model id.
func ValidModel(name string) bool { return modelRe.MatchString(name) }

// FromEnv reads the configuration. Defaults are the decision of
// evals/retrieval: BAAI/bge-m3, 1024 dimensions, threshold 0.51. Without
// HARNESS_EMBED_URL the feature is off and the other variables are not checked.
func FromEnv() (Config, error) { return FromLookup(os.Getenv) }

// FromLookup is FromEnv with the variables read through get (the daemon reads
// them from its env file, not from the process environment).
func FromLookup(get func(string) string) (Config, error) {
	c := Config{URL: get("HARNESS_EMBED_URL"), Model: DefaultModel, Dim: DefaultDim, MinSim: DefaultMinSim}
	if c.URL == "" {
		return c, nil
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return c, fmt.Errorf("HARNESS_EMBED_URL must be an http(s) URL")
	}
	// Spec S2 B.1: embedding runs on this machine; no off-machine service is called.
	if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return c, fmt.Errorf("HARNESS_EMBED_URL must point to loopback (the embedding sidecar is local)")
	}
	if v := get("HARNESS_EMBED_MODEL"); v != "" {
		c.Model = v
	}
	if !ValidModel(c.Model) {
		return c, fmt.Errorf("HARNESS_EMBED_MODEL is not a valid model id")
	}
	if v := get("HARNESS_EMBED_DIM"); v != "" {
		if c.Dim, err = strconv.Atoi(v); err != nil || c.Dim < 1 || c.Dim > 16000 {
			return c, fmt.Errorf("HARNESS_EMBED_DIM must be an integer between 1 and 16000")
		}
	}
	if v := get("HARNESS_EMBED_MIN_SIM"); v != "" {
		if c.MinSim, err = strconv.ParseFloat(v, 64); err != nil || !(c.MinSim > 0 && c.MinSim <= 1) {
			return c, fmt.Errorf("HARNESS_EMBED_MIN_SIM must be a number in (0, 1]")
		}
	}
	return c, nil
}

// Client returns the sidecar client, or nil when semantic retrieval is off.
func (c Config) Client() *Client {
	if c.URL == "" {
		return nil
	}
	return &Client{BaseURL: c.URL, Model: c.Model, Dim: c.Dim}
}
