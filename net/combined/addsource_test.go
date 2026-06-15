package combined

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
)

// stubGetter returns a fixed payload for a fixed CID.
type stubGetter struct {
	want cid.Cid
	data []byte
	hits int
}

func (s *stubGetter) Get(_ context.Context, c cid.Cid) ([]byte, error) {
	if c == s.want {
		s.hits++
		return s.data, nil
	}
	return nil, context.DeadlineExceeded
}

func TestAddSource_PrependAndIdempotent(t *testing.T) {
	f := New(nil) // no cache, no sources
	if got := len(f.snapshotSources()); got != 0 {
		t.Fatalf("expected 0 sources, got %d", got)
	}
	g1 := &stubGetter{}
	f.AddSource(Source{Name: "a", Getter: g1, Timeout: time.Second}, false)
	f.AddSource(Source{Name: "b", Getter: g1, Timeout: time.Second}, true) // prepend
	srcs := f.snapshotSources()
	if len(srcs) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(srcs))
	}
	if srcs[0].Name != "b" || srcs[1].Name != "a" {
		t.Fatalf("prepend order wrong: %s,%s", srcs[0].Name, srcs[1].Name)
	}
	// Idempotent on Name.
	f.AddSource(Source{Name: "a", Getter: g1}, false)
	if len(f.snapshotSources()) != 2 {
		t.Fatalf("duplicate Name added: %d", len(f.snapshotSources()))
	}
}

func TestAddSource_RaceWithGet(t *testing.T) {
	c, _ := cid.Parse("bafy2bzacecnamqgqmifpluoeldx7zzglxcljo6oja4vrmtj7432rphldpdmm2")
	g := &stubGetter{want: c, data: []byte("hi")}
	f := New(nil, Source{Name: "seed", Getter: g, Timeout: time.Second, Race: true})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			f.AddSource(Source{Name: string(rune('A' + (i % 20))), Getter: g, Timeout: time.Second}, i%2 == 0)
		}
	}()
	// Hammer Get concurrently; the race detector will flag unsafe access.
	for i := 0; i < 2000; i++ {
		_, _ = f.Get(context.Background(), c)
	}
	close(stop)
	wg.Wait()
}
