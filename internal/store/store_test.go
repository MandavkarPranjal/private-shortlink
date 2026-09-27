package store

import (
	"context"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSaveGetUpdateDelete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	l := &Link{Name: "blog", URL: "https://example.com", Owner: "alice"}
	if err := s.Save(ctx, l, false); err != nil {
		t.Fatalf("Save create: %v", err)
	}
	if l.Owner != "alice" || l.Clicks != 0 || l.CreatedAt.IsZero() {
		t.Fatalf("unexpected link after create: %+v", l)
	}

	// Duplicate create fails.
	if err := s.Save(ctx, &Link{Name: "blog", URL: "https://x", Owner: "bob"}, false); err != ErrExists {
		t.Fatalf("Save duplicate: got %v, want ErrExists", err)
	}

	got, err := s.Get(ctx, "blog")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.URL != "https://example.com" {
		t.Errorf("URL = %q", got.URL)
	}

	// Update keeps owner.
	upd := &Link{Name: "blog", URL: "https://example.org/new", Owner: "mallory"}
	if err := s.Save(ctx, upd, true); err != nil {
		t.Fatalf("Save update: %v", err)
	}
	if upd.Owner != "alice" {
		t.Errorf("owner changed to %q on update", upd.Owner)
	}
	if upd.URL != "https://example.org/new" {
		t.Errorf("URL after update = %q", upd.URL)
	}

	if err := s.Delete(ctx, "blog"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "blog"); err != ErrNotFound {
		t.Errorf("Get after delete: got %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "blog"); err != ErrNotFound {
		t.Errorf("Delete missing: got %v, want ErrNotFound", err)
	}
}

func TestListSearch(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	seed := []*Link{
		{Name: "a/docs", URL: "https://docs.example.com", Owner: "alice"},
		{Name: "blog", URL: "https://example.com/blog", Owner: "alice"},
		{Name: "go", URL: "https://go.dev", Owner: "bob"},
	}
	for _, l := range seed {
		if err := s.Save(ctx, l, false); err != nil {
			t.Fatalf("Save %s: %v", l.Name, err)
		}
	}

	all, err := s.List(ctx, "")
	if err != nil || len(all) != 3 {
		t.Fatalf("List all: %v, len=%d", err, len(all))
	}

	byName, err := s.List(ctx, "blo")
	if err != nil || len(byName) != 1 || byName[0].Name != "blog" {
		t.Fatalf("List name match: %v, %+v", err, byName)
	}

	byURL, err := s.List(ctx, "go.dev")
	if err != nil || len(byURL) != 1 || byURL[0].Name != "go" {
		t.Fatalf("List url match: %v, %+v", err, byURL)
	}

	// LIKE metacharacters are treated literally.
	if hits, err := s.List(ctx, "%"); err != nil || len(hits) != 0 {
		t.Fatalf("List literal %%: %v, %d hits", err, len(hits))
	}
}

func TestIncClick(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	l := &Link{Name: "x", URL: "https://example.com", Owner: "o"}
	if err := s.Save(ctx, l, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.IncClick(ctx, "x"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Get(ctx, "x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Clicks != 3 {
		t.Errorf("clicks = %d, want 3", got.Clicks)
	}
	// IncClick on missing link is a no-op.
	if err := s.IncClick(ctx, "missing"); err != nil {
		t.Errorf("IncClick missing: %v", err)
	}
}
