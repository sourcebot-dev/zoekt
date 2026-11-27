// Copyright 2018 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package search

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
)

type loggingLoader struct {
	loads chan string
	drops chan string
}

func (l *loggingLoader) load(keys ...string) {
	for _, key := range keys {
		l.loads <- key
	}
}

func (l *loggingLoader) drop(keys ...string) {
	for _, key := range keys {
		l.drops <- key
	}
}

func advanceFS() {
	time.Sleep(10 * time.Millisecond)
}

func TestDirWatcherUnloadOnce(t *testing.T) {
	dir := t.TempDir()

	logger := &loggingLoader{
		loads: make(chan string, 10),
		drops: make(chan string, 10),
	}
	// Upstream fails if empty. Sourcegraph does not
	// _, err := NewDirectoryWatcher(dir, logger)
	// if err == nil || !strings.Contains(err.Error(), "empty") {
	// 	t.Fatalf("got %v, want 'empty'", err)
	// }

	shard := filepath.Join(dir, "foo.zoekt")
	if err := os.WriteFile(shard, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	dw, err := newDirectoryWatcher(dir, logger)
	if err != nil {
		t.Fatalf("NewDirectoryWatcher: %v", err)
	}
	defer dw.Stop()

	if got := <-logger.loads; got != shard {
		t.Fatalf("got load event %v, want %v", got, shard)
	}

	// Must sleep because of FS timestamp resolution.
	advanceFS()
	if err := os.WriteFile(shard, []byte("changed"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if got := <-logger.loads; got != shard {
		t.Fatalf("got load event %v, want %v", got, shard)
	}

	advanceFS()
	if err := os.Remove(shard); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if got := <-logger.drops; got != shard {
		t.Fatalf("got drops event %v, want %v", got, shard)
	}

	advanceFS()
	if err := os.WriteFile(shard+".bla", []byte("changed"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	dw.Stop()

	// fsnotify can produce multiple events for a single write, which can lead to
	// extra queued load notifications by the time we stop. Drain them and only
	// assert we don't drop the same shard again.
	for len(logger.loads) > 0 {
		<-logger.loads
	}

	select {
	case k := <-logger.drops:
		t.Errorf("spurious drops of %q", k)
	default:
	}
}

func TestDirWatcherLoadEmpty(t *testing.T) {
	dir := t.TempDir()

	logger := &loggingLoader{
		loads: make(chan string, 10),
		drops: make(chan string, 10),
	}
	dw, err := newDirectoryWatcher(dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	advanceFS()
	dw.Stop()

	select {
	case k := <-logger.loads:
		t.Errorf("spurious load of %q", k)
	case k := <-logger.drops:
		t.Errorf("spurious drops of %q", k)
	default:
	}
}

func TestVersionFromPath(t *testing.T) {
	cases := map[string]struct {
		name    string
		version int
	}{
		"github.com%2Fgoogle%2Fzoekt_v16.00000.zoekt": {
			name:    "github.com%2Fgoogle%2Fzoekt",
			version: 16,
		},
		"github.com%2Fgoogle%2Fsre_yield_v15.00000.zoekt": {
			name:    "github.com%2Fgoogle%2Fsre_yield",
			version: 15,
		},
		"repos/github.com%2Fgoogle%2Fsre_yield_v15.00000.zoekt": {
			name:    "repos/github.com%2Fgoogle%2Fsre_yield",
			version: 15,
		},
		"foo": {
			name:    "foo",
			version: 0,
		},
		"foo_bar": {
			name:    "foo_bar",
			version: 0,
		},
		"github.com%2Fgoogle%2Fzoekt_vfoo.00000.zoekt": {
			name:    "github.com%2Fgoogle%2Fzoekt_vfoo.00000.zoekt",
			version: 0,
		},
	}
	for path, tc := range cases {
		name, version := versionFromPath(path)
		if name != tc.name || version != tc.version {
			t.Errorf("%s: got name %s and version %d, want name %s and version %d", path, name, version, tc.name, tc.version)
		}
	}
}

func TestDirWatcherLoadLatest(t *testing.T) {
	dir := t.TempDir()

	logger := &loggingLoader{
		loads: make(chan string, 10),
		drops: make(chan string, 10),
	}
	// Upstream fails if empty. Sourcegraph does not
	// _, err := NewDirectoryWatcher(dir, logger)
	// if err == nil || !strings.Contains(err.Error(), "empty") {
	// 	t.Fatalf("got %v, want 'empty'", err)
	// }

	want := index.NextIndexFormatVersion
	shardLatest := filepath.Join(dir, fmt.Sprintf("foo_v%d.00000.zoekt", want))

	for delta := -1; delta <= 1; delta++ {
		repo := fmt.Sprintf("foo_v%d.00000.zoekt", want+delta)
		shard := filepath.Join(dir, repo)
		if err := os.WriteFile(shard, []byte("hello"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	dw, err := newDirectoryWatcher(dir, logger)
	if err != nil {
		t.Fatalf("NewDirectoryWatcher: %v", err)
	}
	defer dw.Stop()

	if got := <-logger.loads; got != shardLatest {
		t.Fatalf("got load event %v, want %v", got, shardLatest)
	}

	advanceFS()
	dw.Stop()

	select {
	case k := <-logger.loads:
		t.Errorf("spurious load of %q", k)
	case k := <-logger.drops:
		t.Errorf("spurious drops of %q", k)
	default:
	}
}

// A normal build renames its shards into place before it removes the .meta files that a delta
// build left next to the shards they replace. A searcher that loads a new shard in between reads
// the old .meta file with it, so it has to load the shard again once that file is gone.
func TestDirWatcherReloadsShardWhenMetaIsRemoved(t *testing.T) {
	build := func(dir string, delta bool, files map[string]string) {
		t.Helper()

		b, err := index.NewBuilder(index.Options{
			IndexDir:              dir,
			RepositoryDescription: zoekt.Repository{Name: "repo", ID: 1},
			DisableCTags:          true,
			IsDelta:               delta,
		})
		if err != nil {
			t.Fatalf("NewBuilder: %v", err)
		}
		for name, content := range files {
			if delta {
				b.MarkFileAsChangedOrRemoved(name)
			}
			if err := b.AddFile(name, []byte(content)); err != nil {
				t.Fatalf("AddFile: %v", err)
			}
		}
		if err := b.Finish(); err != nil {
			t.Fatalf("Finish: %v", err)
		}
	}

	// documents returns what the searcher finds, as sorted "name: content" lines.
	documents := func(s zoekt.Searcher) []string {
		t.Helper()

		result, err := s.Search(context.Background(), &query.Const{Value: true}, &zoekt.SearchOptions{Whole: true})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}

		var docs []string
		for _, f := range result.Files {
			docs = append(docs, fmt.Sprintf("%s: %s", f.FileName, f.Content))
		}
		sort.Strings(docs)
		return docs
	}

	dir := t.TempDir()
	shard := func(n int) string {
		return filepath.Join(dir, fmt.Sprintf("repo_v%d.%05d.zoekt", index.IndexFormatVersion, n))
	}

	// The delta build puts the new b.txt in shard 1, and a tombstone for the old one in the .meta
	// file of shard 0.
	build(dir, false, map[string]string{"a.txt": "apple", "b.txt": "banana"})
	build(dir, true, map[string]string{"b.txt": "blueberry"})

	live := newShardedSearcher(1)
	t.Cleanup(live.Close)

	dw, err := newDirectoryWatcher(dir, &loader{ss: live})
	if err != nil {
		t.Fatalf("newDirectoryWatcher: %v", err)
	}
	if err := dw.WaitUntilReady(); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	// From here on the test decides when the searcher looks at the directory.
	dw.Stop()

	// A normal build of the same files writes one shard. Publish it the way Builder.Finish does,
	// with the searcher looking at the directory after each step.
	scratch := t.TempDir()
	build(scratch, false, map[string]string{"a.txt": "apple", "b.txt": "blueberry"})

	if err := os.Rename(filepath.Join(scratch, filepath.Base(shard(0))), shard(0)); err != nil {
		t.Fatal(err)
	}
	if err := dw.scan(); err != nil {
		t.Fatal(err)
	}

	for _, old := range []string{shard(0) + ".meta", shard(1)} {
		if err := os.Remove(old); err != nil {
			t.Fatal(err)
		}
	}
	if err := dw.scan(); err != nil {
		t.Fatal(err)
	}

	fresh, err := NewDirectorySearcher(dir)
	if err != nil {
		t.Fatalf("NewDirectorySearcher: %v", err)
	}
	t.Cleanup(fresh.Close)

	if diff := cmp.Diff(documents(fresh), documents(live)); diff != "" {
		t.Errorf("the running searcher differs from a new one (-new +running):\n%s", diff)
	}
}

func TestHumanTruncateList(t *testing.T) {
	paths := []string{
		"dir/1",
		"dir/2",
		"dir/3",
		"dir/4",
	}

	assert := func(max int, want string) {
		got := humanTruncateList(paths, max)
		if got != want {
			t.Errorf("unexpected humanTruncateList max=%d.\ngot:  %s\nwant: %s", max, got, want)
		}
	}

	assert(1, "1... 3 more")
	assert(2, "1, 2... 2 more")
	assert(3, "1, 2, 3... 1 more")
	assert(4, "1, 2, 3, 4")
	assert(5, "1, 2, 3, 4")
}
