package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTagSelectorSelect_List(t *testing.T) {
	sel := TagSelector{List: []string{"latest", "3.12", "nope"}}
	got, missing, err := sel.Select([]string{"latest", "3.12", "3.13"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"latest", "3.12"}; !reflect.DeepEqual(got, want) {
		t.Errorf("selected = %v, want %v", got, want)
	}
	if want := []string{"nope"}; !reflect.DeepEqual(missing, want) {
		t.Errorf("missing = %v, want %v", missing, want)
	}
}

func TestTagSelectorSelect_AllWithRegex(t *testing.T) {
	sel := TagSelector{All: true, Include: `^[0-9]+$`, Exclude: `^18$`}
	got, _, err := sel.Select([]string{"20", "22", "18", "latest", "20-dev"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"20", "22"}; !reflect.DeepEqual(got, want) {
		t.Errorf("selected = %v, want %v", got, want)
	}
}

func TestTagSelectorSelect_BadRegex(t *testing.T) {
	if _, _, err := (TagSelector{All: true, Include: "("}).Select([]string{"x"}); err == nil {
		t.Fatal("expected error for invalid regex")
	}
}

func TestDestRepo(t *testing.T) {
	cases := []struct {
		name, dest, want string
	}{
		{"python", "reg.example.com/mirror", "reg.example.com/mirror/python"},
		{"nginx", "reg.example.com/mirror/nginx", "reg.example.com/mirror/nginx"}, // explicit full path
	}
	for _, c := range cases {
		r := Repository{Name: c.name, Destination: c.dest}
		if got := r.DestRepo(); got != c.want {
			t.Errorf("DestRepo(%q,%q) = %q, want %q", c.name, c.dest, got, c.want)
		}
	}
}

func TestLoadResolvesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	yaml := `
defaults:
  source: cgr.dev/example.com
  destination: reg.example.com/mirror
  tags:
    list: ["latest"]
repositories:
  - name: python
  - name: node
    tags:
      all: true
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	py := cfg.Repositories[0]
	if py.SourceRepo() != "cgr.dev/example.com/python" {
		t.Errorf("python source = %q", py.SourceRepo())
	}
	if got := py.Tags.List; !reflect.DeepEqual(got, []string{"latest"}) {
		t.Errorf("python inherited tags = %v", got)
	}
	if !cfg.Repositories[1].Tags.All {
		t.Errorf("node should keep its own all:true selector")
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(path, []byte("repositories:\n  - name: x\n    bogus: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for unknown key")
	}
}
