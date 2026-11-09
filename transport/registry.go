package transport

import (
	"regexp"
	"sort"
	"sync"
)

type Config struct {
	StateDir string
	Settings map[string]string
}
type Factory func(Config) (Transport, error)

var registry = struct {
	sync.RWMutex
	entries map[string]Factory
}{entries: make(map[string]Factory)}
var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func Register(name string, f Factory) error {
	registry.Lock()
	defer registry.Unlock()
	if !nameRE.MatchString(name) || f == nil {
		return NewError(InvalidReference)
	}
	if _, ok := registry.entries[name]; ok {
		return NewError(Refused)
	}
	registry.entries[name] = f
	return nil
}
func Lookup(name string) (Factory, bool) {
	registry.RLock()
	defer registry.RUnlock()
	f, ok := registry.entries[name]
	return f, ok
}
func Names() []string {
	registry.RLock()
	defer registry.RUnlock()
	out := make([]string, 0, len(registry.entries))
	for n := range registry.entries {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
