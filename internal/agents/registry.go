// Package agents loads declarative ACP agent profiles.
package agents

import (
	"bytes"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

var (
	//go:embed defaults/*.yaml
	defaultProfiles embed.FS
	namePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	envNamePattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Profile is the complete V1 agent registry schema.
type Profile struct {
	Name        string            `yaml:"name" json:"name"`
	Provider    string            `yaml:"provider" json:"provider"`
	Command     string            `yaml:"command" json:"command"`
	Args        []string          `yaml:"args,omitempty" json:"args,omitempty"`
	Environment map[string]string `yaml:"environment,omitempty" json:"environment,omitempty"`
}

// Argv returns a fresh argv slice suitable for exec.CommandContext. No shell is
// involved, so profile values are never interpolated.
func (p Profile) Argv() []string {
	return append([]string{p.Command}, p.Args...)
}

func (p Profile) Validate() error {
	if !namePattern.MatchString(p.Name) {
		return fmt.Errorf("invalid agent name %q", p.Name)
	}
	if strings.TrimSpace(p.Provider) == "" {
		return fmt.Errorf("agent %q provider is required", p.Name)
	}
	if strings.TrimSpace(p.Command) == "" || strings.ContainsRune(p.Command, '\x00') {
		return fmt.Errorf("agent %q command is invalid", p.Name)
	}
	for index, arg := range p.Args {
		if strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("agent %q argument %d contains a NUL byte", p.Name, index)
		}
	}
	for name, value := range p.Environment {
		if !envNamePattern.MatchString(name) {
			return fmt.Errorf("agent %q environment name %q is invalid", p.Name, name)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("agent %q environment %q contains a NUL byte", p.Name, name)
		}
	}
	return nil
}

type Registry struct {
	profiles map[string]Profile
}

// Load reads embedded profiles and overlays user-owned *.yaml profiles in
// lexical order. A user profile with the same name replaces its default.
func Load(userDirectory string) (*Registry, error) {
	profiles := make(map[string]Profile)
	if err := loadFS(defaultProfiles, "defaults", profiles, false); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(userDirectory)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read agent registry %q: %w", userDirectory, err)
		}
		return &Registry{profiles: profiles}, nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	seenUser := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		path := filepath.Join(userDirectory, entry.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read agent profile %q: %w", path, err)
		}
		profile, err := decodeProfile(body)
		if err != nil {
			return nil, fmt.Errorf("load agent profile %q: %w", path, err)
		}
		if previous, exists := seenUser[profile.Name]; exists {
			return nil, fmt.Errorf("agent %q is defined by both %q and %q", profile.Name, previous, path)
		}
		seenUser[profile.Name] = path
		profiles[profile.Name] = profile
	}
	return &Registry{profiles: profiles}, nil
}

func loadFS(source fs.FS, directory string, profiles map[string]Profile, allowOverlay bool) error {
	entries, err := fs.ReadDir(source, directory)
	if err != nil {
		return fmt.Errorf("read embedded agent profiles: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		body, err := fs.ReadFile(source, directory+"/"+entry.Name())
		if err != nil {
			return fmt.Errorf("read embedded agent profile %q: %w", entry.Name(), err)
		}
		profile, err := decodeProfile(body)
		if err != nil {
			return fmt.Errorf("load embedded agent profile %q: %w", entry.Name(), err)
		}
		if _, exists := profiles[profile.Name]; exists && !allowOverlay {
			return fmt.Errorf("duplicate embedded agent %q", profile.Name)
		}
		profiles[profile.Name] = profile
	}
	return nil
}

func decodeProfile(body []byte) (Profile, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	decoder.KnownFields(true)
	var profile Profile
	if err := decoder.Decode(&profile); err != nil {
		return Profile{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Profile{}, fmt.Errorf("multiple YAML documents are not supported")
		}
		return Profile{}, err
	}
	if err := profile.Validate(); err != nil {
		return Profile{}, err
	}
	profile.Args = append([]string(nil), profile.Args...)
	if len(profile.Environment) > 0 {
		copyEnvironment := make(map[string]string, len(profile.Environment))
		for name, value := range profile.Environment {
			copyEnvironment[name] = value
		}
		profile.Environment = copyEnvironment
	}
	return profile, nil
}

func (r *Registry) Get(name string) (Profile, error) {
	profile, ok := r.profiles[name]
	if !ok {
		return Profile{}, fmt.Errorf("agent profile %q was not found", name)
	}
	return profile, nil
}

func (r *Registry) List() []Profile {
	result := make([]Profile, 0, len(r.profiles))
	for _, profile := range r.profiles {
		result = append(result, profile)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}
