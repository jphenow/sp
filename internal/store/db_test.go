package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testDB creates a temporary SQLite database for testing and returns
// the DB handle along with a cleanup function.
func testDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := OpenPath(path)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenAndMigrate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	db, err := OpenPath(path)
	if err != nil {
		t.Fatalf("initial open: %v", err)
	}
	db.Close()

	// Verify the file exists
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file not created: %v", err)
	}

	// Re-open should succeed (migrations are idempotent)
	db2, err := OpenPath(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	db2.Close()
}

func TestUpsertAndGetSprite(t *testing.T) {
	db := testDB(t)

	s := &Sprite{
		Name:       "gh-test--repo",
		LocalPath:  "/home/user/repo",
		RemotePath: "/home/sprite/repo",
		Repo:       "test/repo",
		Org:        "test-org",
		SpriteID:   "sprite-abc123",
		URL:        "https://gh-test--repo-org.sprites.app",
		Status:     "running",
		SyncStatus: "watching",
	}

	if err := db.UpsertSprite(s); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := db.GetSprite("gh-test--repo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("sprite not found after upsert")
	}
	if got.Name != s.Name {
		t.Errorf("name = %q, want %q", got.Name, s.Name)
	}
	if got.LocalPath != s.LocalPath {
		t.Errorf("local_path = %q, want %q", got.LocalPath, s.LocalPath)
	}
	if got.Status != "running" {
		t.Errorf("status = %q, want %q", got.Status, "running")
	}
	if got.SyncStatus != "watching" {
		t.Errorf("sync_status = %q, want %q", got.SyncStatus, "watching")
	}
}

func TestUpsertMergesNonEmptyFields(t *testing.T) {
	db := testDB(t)

	// Insert initial sprite
	if err := db.UpsertSprite(&Sprite{
		Name:       "test-sprite",
		LocalPath:  "/original/path",
		Repo:       "original/repo",
		Status:     "running",
		SyncStatus: "watching",
	}); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}

	// Upsert with partial data - should merge, not overwrite with empties
	if err := db.UpsertSprite(&Sprite{
		Name:   "test-sprite",
		Status: "warm",
	}); err != nil {
		t.Fatalf("partial upsert: %v", err)
	}

	got, err := db.GetSprite("test-sprite")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LocalPath != "/original/path" {
		t.Errorf("local_path should be preserved, got %q", got.LocalPath)
	}
	if got.Repo != "original/repo" {
		t.Errorf("repo should be preserved, got %q", got.Repo)
	}
	if got.Status != "warm" {
		t.Errorf("status should be updated to 'warm', got %q", got.Status)
	}
	// SyncStatus was 'watching' and new value is 'none' (default) which should NOT overwrite
	if got.SyncStatus != "watching" {
		t.Errorf("sync_status should be preserved as 'watching', got %q", got.SyncStatus)
	}
}

func TestGetSpriteNotFound(t *testing.T) {
	db := testDB(t)

	got, err := db.GetSprite("nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Error("expected nil for nonexistent sprite")
	}
}

func TestListSpritesEmpty(t *testing.T) {
	db := testDB(t)

	sprites, err := db.ListSprites(ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sprites) != 0 {
		t.Errorf("expected 0 sprites, got %d", len(sprites))
	}
}

func TestListSpritesWithFilters(t *testing.T) {
	db := testDB(t)

	sprites := []*Sprite{
		{Name: "gh-superfly--flyctl", LocalPath: "/workspace/superfly/flyctl", Repo: "superfly/flyctl", Status: "running", SyncStatus: "watching"},
		{Name: "gh-superfly--mpg-ui", LocalPath: "/workspace/superfly/mpg-ui", Repo: "superfly/mpg-ui", Status: "warm", SyncStatus: "none"},
		{Name: "gh-jphenow--gameservers", LocalPath: "/home/user/Code/gameservers", Repo: "jphenow/gameservers", Status: "running", SyncStatus: "watching"},
	}
	for _, s := range sprites {
		if err := db.UpsertSprite(s); err != nil {
			t.Fatalf("upsert %q: %v", s.Name, err)
		}
		// Small sleep to ensure different updated_at for ordering
		time.Sleep(10 * time.Millisecond)
	}

	// Tag the superfly sprites as "work"
	for _, name := range []string{"gh-superfly--flyctl", "gh-superfly--mpg-ui"} {
		if err := db.AddTag(name, "work"); err != nil {
			t.Fatalf("add tag: %v", err)
		}
	}
	if err := db.AddTag("gh-jphenow--gameservers", "personal"); err != nil {
		t.Fatalf("add tag: %v", err)
	}

	t.Run("no filter returns all", func(t *testing.T) {
		got, err := db.ListSprites(ListOptions{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("expected 3 sprites, got %d", len(got))
		}
	})

	t.Run("filter by tag", func(t *testing.T) {
		got, err := db.ListSprites(ListOptions{Tags: []string{"work"}})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("expected 2 work sprites, got %d", len(got))
		}
	})

	t.Run("filter by path prefix", func(t *testing.T) {
		got, err := db.ListSprites(ListOptions{PathPrefix: "/workspace/superfly"})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("expected 2 sprites with superfly prefix, got %d", len(got))
		}
	})

	t.Run("filter by name", func(t *testing.T) {
		got, err := db.ListSprites(ListOptions{NameFilter: "flyctl"})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("expected 1 sprite matching 'flyctl', got %d", len(got))
		}
		if len(got) > 0 && got[0].Name != "gh-superfly--flyctl" {
			t.Errorf("wrong sprite: %q", got[0].Name)
		}
	})

	t.Run("combined filters", func(t *testing.T) {
		got, err := db.ListSprites(ListOptions{Tags: []string{"work"}, NameFilter: "mpg"})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("expected 1 sprite, got %d", len(got))
		}
	})
}

func TestUpdateSpriteStatus(t *testing.T) {
	db := testDB(t)

	if err := db.UpsertSprite(&Sprite{Name: "test", Status: "running"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := db.UpdateSpriteStatus("test", "cold"); err != nil {
		t.Fatalf("update status: %v", err)
	}

	got, err := db.GetSprite("test")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != "cold" {
		t.Errorf("status = %q, want %q", got.Status, "cold")
	}
}

func TestUpdateSyncStatus(t *testing.T) {
	db := testDB(t)

	if err := db.UpsertSprite(&Sprite{Name: "test", SyncStatus: "watching"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := db.UpdateSyncStatus("test", "error", "connection refused"); err != nil {
		t.Fatalf("update sync status: %v", err)
	}

	got, err := db.GetSprite("test")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SyncStatus != "error" {
		t.Errorf("sync_status = %q, want %q", got.SyncStatus, "error")
	}
	if got.SyncError != "connection refused" {
		t.Errorf("sync_error = %q, want %q", got.SyncError, "connection refused")
	}
}

func TestDeleteSprite(t *testing.T) {
	db := testDB(t)

	if err := db.UpsertSprite(&Sprite{Name: "doomed"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.AddTag("doomed", "test"); err != nil {
		t.Fatalf("add tag: %v", err)
	}

	if err := db.DeleteSprite("doomed"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	got, err := db.GetSprite("doomed")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != nil {
		t.Error("sprite should be deleted")
	}

	// Tags should be cascade-deleted
	tags, err := db.GetTags("doomed")
	if err != nil {
		t.Fatalf("get tags: %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("tags should be cascade-deleted, got %v", tags)
	}
}

func TestVariantAndPinned(t *testing.T) {
	db := testDB(t)

	// Base sprite and two variants
	base := &Sprite{Name: "gh-fly--flyctl", BaseName: "gh-fly--flyctl", Status: "running"}
	v1 := &Sprite{Name: "gh-fly--flyctl--auth-ideas", BaseName: "gh-fly--flyctl", Variant: "auth-ideas", Status: "running"}
	v2 := &Sprite{Name: "gh-fly--flyctl--retry-thing", BaseName: "gh-fly--flyctl", Variant: "retry-thing", Status: "running"}
	other := &Sprite{Name: "gh-fly--proxy", BaseName: "gh-fly--proxy", Status: "running"}

	for _, s := range []*Sprite{base, v1, v2, other} {
		if err := db.UpsertSprite(s); err != nil {
			t.Fatalf("upsert %q: %v", s.Name, err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Round-trip variant/base_name/pinned
	got, err := db.GetSprite("gh-fly--flyctl--auth-ideas")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Variant != "auth-ideas" {
		t.Errorf("variant = %q, want %q", got.Variant, "auth-ideas")
	}
	if got.BaseName != "gh-fly--flyctl" {
		t.Errorf("base_name = %q, want %q", got.BaseName, "gh-fly--flyctl")
	}
	if got.Pinned {
		t.Error("pinned should default to false")
	}

	t.Run("OnlyVariants", func(t *testing.T) {
		got, err := db.ListSprites(ListOptions{OnlyVariants: true})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("expected 2 variants, got %d", len(got))
		}
	})

	t.Run("VariantsOf", func(t *testing.T) {
		got, err := db.ListSprites(ListOptions{VariantsOf: "gh-fly--flyctl", OnlyVariants: true})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("expected 2 variants of flyctl, got %d", len(got))
		}
	})

	t.Run("SetPinned and OnlyUnpinned", func(t *testing.T) {
		if err := db.SetPinned("gh-fly--flyctl--auth-ideas", true); err != nil {
			t.Fatalf("set pinned: %v", err)
		}
		got, err := db.GetSprite("gh-fly--flyctl--auth-ideas")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if !got.Pinned {
			t.Error("pinned should be true after SetPinned")
		}

		// Only unpinned variants should exclude the pinned one
		unpinned, err := db.ListSprites(ListOptions{OnlyVariants: true, OnlyUnpinned: true})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(unpinned) != 1 {
			t.Errorf("expected 1 unpinned variant, got %d", len(unpinned))
		}
		if len(unpinned) > 0 && unpinned[0].Variant != "retry-thing" {
			t.Errorf("unexpected unpinned variant: %q", unpinned[0].Variant)
		}
	})

	t.Run("SetPinned nonexistent", func(t *testing.T) {
		if err := db.SetPinned("nonexistent", true); err == nil {
			t.Error("expected error for nonexistent sprite")
		}
	})

	t.Run("OlderThan", func(t *testing.T) {
		// Everything is very recent; a far-future threshold catches all
		future := time.Now().Add(1 * time.Hour)
		got, err := db.ListSprites(ListOptions{OlderThan: future})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 4 {
			t.Errorf("expected 4 sprites older than future, got %d", len(got))
		}

		// A past threshold catches nothing
		past := time.Now().Add(-1 * time.Hour)
		got, err = db.ListSprites(ListOptions{OlderThan: past})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("expected 0 sprites older than past, got %d", len(got))
		}
	})
}

func TestSyncSession(t *testing.T) {
	db := testDB(t)

	// Need a parent sprite first
	if err := db.UpsertSprite(&Sprite{Name: "sync-test"}); err != nil {
		t.Fatalf("upsert sprite: %v", err)
	}

	ss := &SyncSession{
		SpriteName:     "sync-test",
		MutagenID:      "sync_abc123",
		SSHPort:        12345,
		ProxyPID:       9999,
		AlphaConnected: true,
		BetaConnected:  true,
		Conflicts:      0,
	}

	if err := db.UpsertSyncSession(ss); err != nil {
		t.Fatalf("upsert sync session: %v", err)
	}

	got, err := db.GetSyncSession("sync-test")
	if err != nil {
		t.Fatalf("get sync session: %v", err)
	}
	if got == nil {
		t.Fatal("sync session not found")
	}
	if got.MutagenID != "sync_abc123" {
		t.Errorf("mutagen_id = %q, want %q", got.MutagenID, "sync_abc123")
	}
	if got.SSHPort != 12345 {
		t.Errorf("ssh_port = %d, want %d", got.SSHPort, 12345)
	}
	if !got.AlphaConnected {
		t.Error("alpha should be connected")
	}

	// Update sync session
	ss.BetaConnected = false
	ss.Conflicts = 2
	ss.LastError = "conflict detected"
	if err := db.UpsertSyncSession(ss); err != nil {
		t.Fatalf("update sync session: %v", err)
	}

	got, err = db.GetSyncSession("sync-test")
	if err != nil {
		t.Fatalf("get updated: %v", err)
	}
	if got.BetaConnected {
		t.Error("beta should be disconnected after update")
	}
	if got.Conflicts != 2 {
		t.Errorf("conflicts = %d, want 2", got.Conflicts)
	}

	// Delete sync session
	if err := db.DeleteSyncSession("sync-test"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err = db.GetSyncSession("sync-test")
	if err != nil {
		t.Fatalf("get deleted: %v", err)
	}
	if got != nil {
		t.Error("sync session should be deleted")
	}
}

func TestTags(t *testing.T) {
	db := testDB(t)

	if err := db.UpsertSprite(&Sprite{Name: "tagged-sprite"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Add tags
	for _, tag := range []string{"work", "frontend", "priority"} {
		if err := db.AddTag("tagged-sprite", tag); err != nil {
			t.Fatalf("add tag %q: %v", tag, err)
		}
	}

	// Duplicate add should be a no-op
	if err := db.AddTag("tagged-sprite", "work"); err != nil {
		t.Fatalf("duplicate add: %v", err)
	}

	tags, err := db.GetTags("tagged-sprite")
	if err != nil {
		t.Fatalf("get tags: %v", err)
	}
	if len(tags) != 3 {
		t.Errorf("expected 3 tags, got %d: %v", len(tags), tags)
	}

	// Remove a tag
	if err := db.RemoveTag("tagged-sprite", "frontend"); err != nil {
		t.Fatalf("remove tag: %v", err)
	}

	tags, err = db.GetTags("tagged-sprite")
	if err != nil {
		t.Fatalf("get tags after remove: %v", err)
	}
	if len(tags) != 2 {
		t.Errorf("expected 2 tags after remove, got %d: %v", len(tags), tags)
	}

	// List all tags
	if err := db.UpsertSprite(&Sprite{Name: "another-sprite"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.AddTag("another-sprite", "personal"); err != nil {
		t.Fatalf("add tag: %v", err)
	}

	allTags, err := db.ListAllTags()
	if err != nil {
		t.Fatalf("list all tags: %v", err)
	}
	if len(allTags) != 3 { // personal, priority, work
		t.Errorf("expected 3 unique tags, got %d: %v", len(allTags), allTags)
	}
}
