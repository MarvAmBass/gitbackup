package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"
)

const header = `# gitbackup configuration
#
# Edited by the web UI and by hand – changes to this file are picked up while
# gitbackup runs (an invalid file is ignored and reported in the UI).
# Comments are kept when the UI saves.
#
# Durations: 90s, 30m, 6h, 2d, 1w.  Tokens: literal or ${ENV_VAR}.
# Hooks get the event as env vars (GB_EVENT, GB_REPO, GB_PATH, GB_REF, GB_OLD,
# GB_NEW, GB_VERSION, GB_DETAIL) and as JSON on stdin; "events" takes event kinds,
# "history" (anything upstream removed or rewrote) or "*".
`

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = v
	return nil
}

// File is the live configuration backed by gitbackup.yaml.
type File struct {
	path     string
	onChange func(*Config)

	mu      sync.Mutex
	cfg     *Config
	doc     *yaml.Node // last loaded document, for comment preservation
	sum     [32]byte
	loadErr error
	loaded  time.Time
}

// Open loads path, creating it with defaults if it does not exist.
func Open(path string) (*File, error) {
	f := &File{path: path}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		f.cfg = Default()
		if err := f.write(f.cfg); err != nil {
			return nil, err
		}
	}
	if err := f.reload(); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *File) Path() string { return f.path }

// OnChange registers the callback run after every successful (re)load or save.
func (f *File) OnChange(fn func(*Config)) { f.mu.Lock(); f.onChange = fn; f.mu.Unlock() }

// Get returns the current configuration. It is a shared snapshot: read it,
// never modify it (use Update).
func (f *File) Get() *Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

// Error is the last load error of a hand-edited file (nil if fine).
func (f *File) Error() error { f.mu.Lock(); defer f.mu.Unlock(); return f.loadErr }

// Update applies fn to a copy, validates and saves it.
func (f *File) Update(fn func(*Config) error) error {
	f.mu.Lock()
	c := clone(f.cfg)
	f.mu.Unlock()
	if err := fn(c); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if err := f.write(c); err != nil {
		return err
	}
	f.mu.Lock()
	f.cfg, f.loadErr = c, nil
	cb := f.onChange
	f.mu.Unlock()
	if cb != nil {
		cb(clone(c))
	}
	return nil
}

// Watch polls the file and reloads it when it changes on disk.
func (f *File) Watch(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b, err := os.ReadFile(f.path)
			if err != nil {
				continue
			}
			f.mu.Lock()
			same := sha256.Sum256(b) == f.sum
			f.mu.Unlock()
			if !same {
				f.reload()
			}
		}
	}
}

func (f *File) reload() error {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	c := &Config{}
	err = yaml.Unmarshal(b, &doc)
	if err == nil {
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		err = dec.Decode(c)
	}
	if err == nil {
		err = c.Validate()
	}
	f.mu.Lock()
	f.sum = sha256.Sum256(b)
	if err != nil {
		f.loadErr = fmt.Errorf("%s: %w", filepath.Base(f.path), err)
		first := f.cfg == nil
		f.mu.Unlock()
		if first {
			return f.loadErr
		}
		return nil // keep running with the last good config
	}
	f.cfg, f.doc, f.loadErr, f.loaded = c, &doc, nil, time.Now()
	cb := f.onChange
	f.mu.Unlock()
	if cb != nil {
		cb(clone(c))
	}
	return nil
}

// write encodes c, carries comments over from the last loaded document and
// replaces the file atomically.
func (f *File) write(c *Config) error {
	var n yaml.Node
	if err := n.Encode(c); err != nil {
		return err
	}
	f.mu.Lock()
	old := f.doc
	f.mu.Unlock()
	if old != nil && len(old.Content) > 0 {
		mergeComments(old.Content[0], &n)
		n.HeadComment = old.Content[0].HeadComment
		if n.HeadComment == "" {
			n.HeadComment = old.HeadComment
		}
	} else {
		n.HeadComment = header
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&n); err != nil {
		return err
	}
	enc.Close()
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, f.path); err != nil {
		return err
	}
	var doc yaml.Node
	yaml.Unmarshal(buf.Bytes(), &doc)
	f.mu.Lock()
	f.sum, f.doc = sha256.Sum256(buf.Bytes()), &doc
	f.mu.Unlock()
	return nil
}

// mergeComments copies comments from old onto matching nodes of new: mapping
// entries by key, sequence items by their identity field.
func mergeComments(old, nw *yaml.Node) {
	if old == nil || nw == nil {
		return
	}
	nw.HeadComment, nw.LineComment, nw.FootComment = pick(nw.HeadComment, old.HeadComment), pick(nw.LineComment, old.LineComment), pick(nw.FootComment, old.FootComment)
	switch {
	case old.Kind == yaml.MappingNode && nw.Kind == yaml.MappingNode:
		for i := 0; i+1 < len(nw.Content); i += 2 {
			for j := 0; j+1 < len(old.Content); j += 2 {
				if old.Content[j].Value == nw.Content[i].Value {
					mergeComments(old.Content[j], nw.Content[i])
					mergeComments(old.Content[j+1], nw.Content[i+1])
					break
				}
			}
		}
	case old.Kind == yaml.SequenceNode && nw.Kind == yaml.SequenceNode:
		for _, item := range nw.Content {
			if o := findItem(old, identity(item)); o != nil {
				mergeComments(o, item)
			}
		}
	}
}

func pick(cur, old string) string {
	if cur != "" {
		return cur
	}
	return old
}

// identity is the value of the first identifying key of a mapping item.
func identity(n *yaml.Node) string {
	if n.Kind != yaml.MappingNode {
		return n.Value
	}
	for _, key := range []string{"name", "host", "repo", "target"} {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				id := key + "=" + n.Content[i+1].Value
				if key == "target" { // sources: target alone is not unique across hosts
					for k := 0; k+1 < len(n.Content); k += 2 {
						if n.Content[k].Value == "host" {
							id += "@" + n.Content[k+1].Value
						}
					}
				}
				return id
			}
		}
	}
	return ""
}

func findItem(seq *yaml.Node, id string) *yaml.Node {
	if id == "" {
		return nil
	}
	for _, it := range seq.Content {
		if identity(it) == id {
			return it
		}
	}
	return nil
}

func clone(c *Config) *Config {
	if c == nil {
		return nil
	}
	b, _ := yaml.Marshal(c)
	out := &Config{}
	yaml.Unmarshal(b, out)
	return out
}
