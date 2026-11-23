package notes

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Memory is an in-memory Store for tests. Like a versioned bucket it keeps
// every generation and never deletes one.
type Memory struct {
	mu   sync.Mutex
	next int64
	now  func() time.Time
	by   map[string][]Note // generations in order; the last is current
}

// NewMemory returns an empty store. Generations start at 1001 and grow by one
// across the whole store.
func NewMemory() *Memory {
	return &Memory{next: 1000, now: time.Now, by: map[string][]Note{}}
}

func (m *Memory) List(_ context.Context, versions bool) ([]Note, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Note
	for _, vs := range m.by {
		for i, n := range vs {
			n.Body = nil
			n.Current = i == len(vs)-1
			if versions || n.Current {
				out = append(out, n)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Generation < out[j].Generation
	})
	return out, nil
}

func (m *Memory) Read(_ context.Context, name string, generation int64) (*Note, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	vs := m.by[name]
	for i, n := range vs {
		if generation == 0 && i == len(vs)-1 || n.Generation == generation {
			n.Current = i == len(vs)-1
			n.Body = append([]byte{}, n.Body...)
			return &n, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) Write(_ context.Context, name string, body []byte, ifGeneration int64) (*Note, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	if len(body) > MaxSize {
		return nil, ErrTooLarge
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	vs := m.by[name]
	var cur int64
	if len(vs) > 0 {
		cur = vs[len(vs)-1].Generation
	}
	if ifGeneration != cur {
		return nil, &ConflictError{Name: name, If: ifGeneration, Current: cur}
	}
	m.next++
	n := Note{Name: name, Generation: m.next, Size: int64(len(body)), Updated: m.now().UTC(), Current: true, Body: append([]byte{}, body...)}
	m.by[name] = append(vs, n)
	out := n
	out.Body = nil
	return &out, nil
}
