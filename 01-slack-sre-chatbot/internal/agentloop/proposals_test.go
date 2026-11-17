package agentloop

import (
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a now func a test can move forward.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestStoreExpiry(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	st := NewStore(30*time.Minute, c.now)
	id := st.Put(Proposal{Env: "prod"})
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(id) {
		t.Fatalf("id = %q, want 16 hex chars", id)
	}
	p, ok := st.Get(id)
	if !ok || p.ID != id || !p.Created.Equal(c.now()) || p.Env != "prod" {
		t.Fatalf("Get = %+v, %v", p, ok)
	}

	c.advance(29 * time.Minute)
	if _, ok := st.Get(id); !ok {
		t.Fatal("expired after 29m, want alive")
	}
	c.advance(2 * time.Minute)
	if _, ok := st.Get(id); ok {
		t.Fatal("alive after 31m, want expired")
	}
	if _, ok := st.Take(id); ok {
		t.Fatal("Take returned an expired proposal")
	}
}

func TestStoreTakeIsOnce(t *testing.T) {
	st := NewStore(time.Hour, time.Now)
	id := st.Put(Proposal{Env: "prod"})
	if p, ok := st.Take(id); !ok || p.ID != id {
		t.Fatalf("first Take = %+v, %v", p, ok)
	}
	if _, ok := st.Take(id); ok {
		t.Fatal("second Take succeeded")
	}
	if _, ok := st.Get(id); ok {
		t.Fatal("Get after Take succeeded")
	}
}

// A double click on the confirm button must open exactly one PR.
func TestStoreTakeConcurrent(t *testing.T) {
	st := NewStore(time.Hour, nil)
	id := st.Put(Proposal{Env: "prod"})
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok := st.Take(id); ok {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := wins.Load(); n != 1 {
		t.Fatalf("Take succeeded %d times, want 1", n)
	}
}

func TestStoreIDsAreDistinctAndDeleteWorks(t *testing.T) {
	st := NewStore(time.Hour, nil)
	seen := map[string]bool{}
	for range 100 {
		id := st.Put(Proposal{})
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
	for id := range seen {
		st.Delete(id)
		if _, ok := st.Get(id); ok {
			t.Fatalf("Get after Delete succeeded for %q", id)
		}
	}
}

// Put drops expired proposals so a store nobody confirms from does not grow forever.
func TestStorePutDropsExpired(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	st := NewStore(30*time.Minute, c.now)
	st.Put(Proposal{})
	c.advance(time.Hour)
	st.Put(Proposal{})
	st.mu.Lock()
	n := len(st.items)
	st.mu.Unlock()
	if n != 1 {
		t.Fatalf("items = %d, want 1", n)
	}
}
