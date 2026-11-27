package index

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/RoaringBitmap/roaring"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

// The helpers below are shared with the generator that produced the v16/v17
// fixtures in ../testdata/widebranches with the unpatched (64-bit branch mask) writer, so
// they must stay deterministic.

func wideBranchName(i int) string {
	return fmt.Sprintf("br%03d", i)
}

func wideBranchRepo(name string, id uint32, n int) *zoekt.Repository {
	r := &zoekt.Repository{Name: name, ID: id}
	for i := 0; i < n; i++ {
		r.Branches = append(r.Branches, zoekt.RepositoryBranch{
			Name:    wideBranchName(i),
			Version: "v-" + wideBranchName(i),
		})
	}
	return r
}

// wideBranchMembers returns the sorted branch indexes of document k in a
// repository with n branches. Document n is "all.txt", which is in every
// branch.
func wideBranchMembers(k, n int) []int {
	if k == n {
		all := make([]int, n)
		for i := range all {
			all[i] = i
		}
		return all
	}
	set := map[int]bool{k % n: true, (k*7 + 3) % n: true}
	if k%3 == 0 {
		set[0] = true
	}
	var out []int
	for i := range set {
		out = append(out, i)
	}
	sort.Ints(out)
	return out
}

func wideBranchDocName(k, n int) string {
	if k == n {
		return "all.txt"
	}
	return fmt.Sprintf("f%03d.txt", k)
}

func wideBranchDocs(n int) []Document {
	var docs []Document
	for k := 0; k <= n; k++ {
		var branches []string
		for _, i := range wideBranchMembers(k, n) {
			branches = append(branches, wideBranchName(i))
		}
		docs = append(docs, Document{
			Name:     wideBranchDocName(k, n),
			Content:  []byte(fmt.Sprintf("needle%03d common\n", k)),
			Branches: branches,
		})
	}
	return docs
}

// wideBranchSpec describes one repository built with wideBranchDocs.
type wideBranchSpec struct {
	name string
	id   uint32
	n    int
}

// expected returns repo => file => branches for documents that are in at
// least one branch accepted by want. Branches are restricted to the accepted
// ones if restrict is true.
func wideBranchExpected(specs []wideBranchSpec, want func(spec wideBranchSpec, branch int) bool, restrict bool) map[string]map[string][]string {
	out := map[string]map[string][]string{}
	for _, spec := range specs {
		for k := 0; k <= spec.n; k++ {
			var all, matched []string
			for _, i := range wideBranchMembers(k, spec.n) {
				all = append(all, wideBranchName(i))
				if want(spec, i) {
					matched = append(matched, wideBranchName(i))
				}
			}
			if len(matched) == 0 {
				continue
			}
			if out[spec.name] == nil {
				out[spec.name] = map[string][]string{}
			}
			if restrict {
				out[spec.name][wideBranchDocName(k, spec.n)] = matched
			} else {
				out[spec.name][wideBranchDocName(k, spec.n)] = all
			}
		}
	}
	return out
}

func wideBranchSearch(t *testing.T, s zoekt.Searcher, q query.Q) map[string]map[string][]string {
	t.Helper()
	res, err := s.Search(context.Background(), q, &zoekt.SearchOptions{})
	if err != nil {
		t.Fatalf("Search(%s): %v", q, err)
	}
	out := map[string]map[string][]string{}
	for _, f := range res.Files {
		if out[f.Repository] == nil {
			out[f.Repository] = map[string][]string{}
		}
		if _, dup := out[f.Repository][f.FileName]; dup {
			t.Fatalf("Search(%s): duplicate result %s/%s", q, f.Repository, f.FileName)
		}
		out[f.Repository][f.FileName] = f.Branches
	}
	return out
}

// checkWideBranchSearcher runs branch-restricted queries against s, which
// must contain exactly the repositories in specs built with wideBranchDocs.
func checkWideBranchSearcher(t *testing.T, s zoekt.Searcher, specs []wideBranchSpec) {
	t.Helper()
	common := &query.Substring{Pattern: "common", Content: true}
	check := func(desc string, q query.Q, want map[string]map[string][]string) {
		t.Helper()
		got := wideBranchSearch(t, s, q)
		if len(want) == 0 {
			want = map[string]map[string][]string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: query %s\ngot  %v\nwant %v", desc, q, got, want)
		}
	}

	maxN := 0
	for _, spec := range specs {
		maxN = max(maxN, spec.n)
	}

	// No branch filter: every document with all of its branches. The
	// version is the one of the first branch containing the document.
	check("no filter", common, wideBranchExpected(specs, func(wideBranchSpec, int) bool { return true }, false))
	res, err := s.Search(context.Background(), common, &zoekt.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Files {
		if want := "v-" + f.Branches[0]; f.Version != want {
			t.Fatalf("%s/%s: got version %q, want %q", f.Repository, f.FileName, f.Version, want)
		}
	}

	// Exact branch filter for every branch, including those beyond 64.
	for j := 0; j < maxN; j++ {
		check("exact branch", query.NewAnd(common, &query.Branch{Pattern: wideBranchName(j), Exact: true}),
			wideBranchExpected(specs, func(_ wideBranchSpec, i int) bool { return i == j }, true))
	}

	// HEAD is always the first branch.
	check("HEAD", query.NewAnd(common, &query.Branch{Pattern: "HEAD", Exact: true}),
		wideBranchExpected(specs, func(_ wideBranchSpec, i int) bool { return i == 0 }, true))

	// Substring branch filters, e.g. "br1" matches br100-br199.
	for _, pat := range []string{"br1", "br06", "br07", "br19"} {
		check("substring branch "+pat, query.NewAnd(common, &query.Branch{Pattern: pat}),
			wideBranchExpected(specs, func(_ wideBranchSpec, i int) bool { return strings.Contains(wideBranchName(i), pat) }, true))
	}

	// BranchesRepos with branches beyond 64 and branches that do not exist.
	// Unknown branches must not match anything (in particular not HEAD).
	for _, spec := range specs {
		for _, j := range []int{0, 63, 64, 65, 127, 128, 150, spec.n - 1} {
			if j < 0 || j >= spec.n {
				continue
			}
			repos := roaring.BitmapOf(spec.id)
			q := query.NewAnd(common, &query.BranchesRepos{List: []query.BranchRepos{
				{Branch: wideBranchName(j), Repos: repos},
				{Branch: "does-not-exist", Repos: repos},
			}})
			want := wideBranchExpected([]wideBranchSpec{spec}, func(_ wideBranchSpec, i int) bool { return i == j }, false)
			check("BranchesRepos", q, want)
		}
		q := query.NewAnd(common, &query.BranchesRepos{List: []query.BranchRepos{
			{Branch: "does-not-exist", Repos: roaring.BitmapOf(spec.id)},
		}})
		check("BranchesRepos unknown", q, nil)
	}
}

func wideBranchSearcher(t *testing.T, data []byte) zoekt.Searcher {
	t.Helper()
	s, err := NewSearcher(&memSeeker{data})
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func wideBranchWrite(t *testing.T, b *ShardBuilder) []byte {
	t.Helper()
	b.IndexTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.ID = "test"
	var buf bytes.Buffer
	if err := b.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestWideBranches(t *testing.T) {
	for _, n := range []int{1, 3, 64, 65, 128, 129, 200} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			spec := wideBranchSpec{name: "wide", id: 1, n: n}
			data := wideBranchWrite(t, testShardBuilder(t, wideBranchRepo(spec.name, spec.id, n), wideBranchDocs(n)...))

			_, md, err := ReadMetadata(&memSeeker{data})
			if err != nil {
				t.Fatal(err)
			}
			wantWords, wantMinReader := 0, WriteMinFeatureVersion
			if n > 64 {
				wantWords, wantMinReader = (n+63)/64, wideBranchMasksFeatureVersion
			}
			if md.BranchMaskWords != wantWords || md.IndexMinReaderVersion != wantMinReader || md.IndexFormatVersion != IndexFormatVersion {
				t.Fatalf("got metadata words=%d minReader=%d format=%d, want words=%d minReader=%d format=%d",
					md.BranchMaskWords, md.IndexMinReaderVersion, md.IndexFormatVersion, wantWords, wantMinReader, IndexFormatVersion)
			}

			checkWideBranchSearcher(t, wideBranchSearcher(t, data), []wideBranchSpec{spec})
		})
	}
}

func TestWideBranchesStats(t *testing.T) {
	n := 200
	data := wideBranchWrite(t, testShardBuilder(t, wideBranchRepo("wide", 1, n), wideBranchDocs(n)...))
	s := wideBranchSearcher(t, data)
	rl, err := s.List(context.Background(), &query.Const{Value: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Every document has one line.
	var wantDefault, wantOther int64
	for k := 0; k <= n; k++ {
		for _, i := range wideBranchMembers(k, n) {
			if i == 0 {
				wantDefault++
			} else {
				wantOther++
			}
		}
	}
	st := rl.Stats
	if st.Documents != n+1 || st.DefaultBranchNewLinesCount != uint64(wantDefault) || st.OtherBranchesNewLinesCount != uint64(wantOther) {
		t.Fatalf("got docs=%d default=%d other=%d, want docs=%d default=%d other=%d",
			st.Documents, st.DefaultBranchNewLinesCount, st.OtherBranchesNewLinesCount, n+1, wantDefault, wantOther)
	}
}

// TestWideBranchesCompound builds compound shards where the repositories have
// different branch counts, both directly and through Merge/Explode.
func TestWideBranchesCompound(t *testing.T) {
	// The first repository has narrow masks; the second forces the builder to
	// widen the masks of documents that were already added.
	specs := []wideBranchSpec{
		{name: "narrow", id: 1, n: 3},
		{name: "wide200", id: 2, n: 200},
		{name: "wide70", id: 3, n: 70},
		{name: "exact64", id: 4, n: 64},
	}
	var repos []*zoekt.Repository
	var docs [][]Document
	for _, spec := range specs {
		repos = append(repos, wideBranchRepo(spec.name, spec.id, spec.n))
		docs = append(docs, wideBranchDocs(spec.n))
	}

	t.Run("builder", func(t *testing.T) {
		data := wideBranchWrite(t, testShardBuilderCompound(t, repos, docs))
		checkWideBranchSearcher(t, wideBranchSearcher(t, data), specs)
	})

	t.Run("merge-explode", func(t *testing.T) {
		dir := t.TempDir()
		var files []IndexFile
		for i, spec := range specs {
			data := wideBranchWrite(t, testShardBuilder(t, repos[i], docs[i]...))
			fn := filepath.Join(dir, spec.name+"_v16.00000.zoekt")
			if err := os.WriteFile(fn, data, 0o644); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(fn)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			indexFile, err := NewIndexFile(f)
			if err != nil {
				t.Fatal(err)
			}
			defer indexFile.Close()
			files = append(files, indexFile)
		}

		mergeDir := t.TempDir()
		tmpName, dstName, err := Merge(mergeDir, files...)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmpName, dstName); err != nil {
			t.Fatal(err)
		}
		compound, err := os.ReadFile(dstName)
		if err != nil {
			t.Fatal(err)
		}
		_, md, err := ReadMetadata(&memSeeker{compound})
		if err != nil {
			t.Fatal(err)
		}
		if md.BranchMaskWords != 4 || md.IndexFormatVersion != NextIndexFormatVersion {
			t.Fatalf("compound shard: got words=%d format=%d", md.BranchMaskWords, md.IndexFormatVersion)
		}
		checkWideBranchSearcher(t, wideBranchSearcher(t, compound), specs)

		explodeDir := t.TempDir()
		if err := Explode(explodeDir, dstName); err != nil {
			t.Fatal(err)
		}
		for _, spec := range specs {
			matches, err := filepath.Glob(filepath.Join(explodeDir, spec.name+"_v*.zoekt"))
			if err != nil || len(matches) != 1 {
				t.Fatalf("exploded shard for %s: %v %v", spec.name, matches, err)
			}
			data, err := os.ReadFile(matches[0])
			if err != nil {
				t.Fatal(err)
			}
			_, md, err := ReadMetadata(&memSeeker{data})
			if err != nil {
				t.Fatal(err)
			}
			if want := (spec.n + 63) / 64; max(md.BranchMaskWords, 1) != want {
				t.Fatalf("exploded %s: got %d words, want %d", spec.name, md.BranchMaskWords, want)
			}
			checkWideBranchSearcher(t, wideBranchSearcher(t, data), []wideBranchSpec{spec})
		}
	})
}

// TestWideBranchesReadOldFormat reads shards written by the unpatched writer
// (64-bit branch masks, before BranchMaskWords existed). The fixtures were
// generated from zoekt 1c71532 with wideBranchRepo/wideBranchDocs.
func TestWideBranchesReadOldFormat(t *testing.T) {
	r64 := wideBranchRepo("wide64", 1, 64)
	r3 := wideBranchRepo("narrow3", 2, 3)
	for _, tc := range []struct {
		file  string
		specs []wideBranchSpec
		build func() *ShardBuilder
	}{{
		file:  "../testdata/widebranches/wide64_v16.00000.zoekt",
		specs: []wideBranchSpec{{name: "wide64", id: 1, n: 64}},
		build: func() *ShardBuilder {
			return testShardBuilder(t, r64, wideBranchDocs(64)...)
		},
	}, {
		file:  "../testdata/widebranches/compound64_v17.00000.zoekt",
		specs: []wideBranchSpec{{name: "narrow3", id: 2, n: 3}, {name: "wide64", id: 1, n: 64}},
		build: func() *ShardBuilder {
			return testShardBuilderCompound(t, []*zoekt.Repository{r3, r64}, [][]Document{wideBranchDocs(3), wideBranchDocs(64)})
		},
	}} {
		t.Run(filepath.Base(tc.file), func(t *testing.T) {
			old, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			checkWideBranchSearcher(t, wideBranchSearcher(t, old), tc.specs)

			// Shards that fit in 64-bit masks are written exactly as before.
			b := tc.build()
			b.IndexTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			b.ID = "fixture-" + filepath.Base(tc.file)
			var buf bytes.Buffer
			if err := b.Write(&buf); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buf.Bytes(), old) {
				t.Fatalf("shard written by the patched builder differs from the unpatched fixture")
			}
		})
	}
}
