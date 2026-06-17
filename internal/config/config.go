// Package config loads and validates the cgr-sync YAML configuration: which
// source repositories to mirror, which tags, and where to push them.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the top-level cgr-sync configuration.
type Config struct {
	// Defaults applied to every repository unless overridden.
	Defaults Defaults `yaml:"defaults"`
	// Repositories to mirror.
	Repositories []Repository `yaml:"repositories"`
}

// Defaults are inherited by each repository.
type Defaults struct {
	// Source registry+namespace, e.g. "cgr.dev/chriscarty.com".
	Source string `yaml:"source"`
	// Destination registry+namespace, e.g. "us-docker.pkg.dev/proj/mirror".
	Destination string `yaml:"destination"`
	// Tag selection applied when a repository doesn't specify its own.
	Tags TagSelector `yaml:"tags"`
}

// Repository is one image stream to mirror from source to destination.
type Repository struct {
	// Name is the repository path under the source/destination namespaces,
	// e.g. "python" -> "cgr.dev/chriscarty.com/python".
	Name string `yaml:"name"`
	// Source overrides Defaults.Source for this repo (full registry+namespace).
	Source string `yaml:"source"`
	// Destination overrides Defaults.Destination. May be a full repo path
	// (registry+namespace+repo) or just a registry+namespace prefix.
	Destination string `yaml:"destination"`
	// Tags overrides Defaults.Tags for this repo.
	Tags TagSelector `yaml:"tags"`
}

// TagSelector chooses which tags of a repository to mirror. The fields are
// applied in order of specificity: an explicit list wins; otherwise All (with
// an optional regex include/exclude) is used.
type TagSelector struct {
	// List is an explicit set of tags, e.g. ["latest", "3.12"].
	List []string `yaml:"list"`
	// All mirrors every tag in the source repository (subject to Include/Exclude).
	All bool `yaml:"all"`
	// Include is a regex; when set, only matching tags are mirrored.
	Include string `yaml:"include"`
	// Exclude is a regex; matching tags are skipped (applied after Include).
	Exclude string `yaml:"exclude"`
}

// IsZero reports whether the selector specifies nothing.
func (t TagSelector) IsZero() bool {
	return len(t.List) == 0 && !t.All && t.Include == "" && t.Exclude == ""
}

// Load reads, parses, validates, and resolves a config file. After Load, every
// returned Repository has Source, Destination and Tags fully populated.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // reject unknown keys to catch typos early
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.resolve(); err != nil {
		return nil, err
	}
	return &c, nil
}

// resolve fills in per-repo defaults and validates the result.
func (c *Config) resolve() error {
	if len(c.Repositories) == 0 {
		return fmt.Errorf("config has no repositories")
	}
	for i := range c.Repositories {
		r := &c.Repositories[i]
		if r.Name == "" {
			return fmt.Errorf("repositories[%d]: name is required", i)
		}
		r.Name = strings.Trim(r.Name, "/")
		if r.Source == "" {
			r.Source = c.Defaults.Source
		}
		if r.Destination == "" {
			r.Destination = c.Defaults.Destination
		}
		if r.Tags.IsZero() {
			r.Tags = c.Defaults.Tags
		}
		r.Source = strings.TrimRight(r.Source, "/")
		r.Destination = strings.TrimRight(r.Destination, "/")
		if r.Source == "" {
			return fmt.Errorf("repository %q: no source (set defaults.source or repository.source)", r.Name)
		}
		if r.Destination == "" {
			return fmt.Errorf("repository %q: no destination (set defaults.destination or repository.destination)", r.Name)
		}
		if r.Tags.IsZero() {
			return fmt.Errorf("repository %q: no tag selector (set defaults.tags or repository.tags)", r.Name)
		}
	}
	return nil
}

// SourceRepo returns the fully-qualified source repository path, e.g.
// "cgr.dev/chriscarty.com/python".
func (r Repository) SourceRepo() string {
	return r.Source + "/" + r.Name
}

// DestRepo returns the fully-qualified destination repository path. If the
// configured Destination already ends with the repo name it is used as-is
// (an explicit per-repo target); otherwise Name is appended to the prefix.
func (r Repository) DestRepo() string {
	if strings.HasSuffix(r.Destination, "/"+r.Name) || r.Destination == r.Name {
		return r.Destination
	}
	return r.Destination + "/" + r.Name
}
