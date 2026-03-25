package art

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// setupTestDir creates a temporary art directory structure for testing.
func setupTestDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	// Create directory structure:
	// animals/
	//   dragon.txt
	//   unicorn.ans
	// funny/
	//   laugh.txt
	//   joke.asc
	// toplevel.txt
	// .git/
	//   config  (should be skipped)

	dirs := []string{
		"animals",
		"funny",
		".git",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0755); err != nil {
			t.Fatal(err)
		}
	}

	files := map[string]string{
		"animals/dragon.txt":  "line1\nline2\nline3\n",
		"animals/unicorn.ans": "sparkle\n",
		"funny/laugh.txt":     "haha\n",
		"funny/joke.asc":      "knock knock\nwho's there\n",
		"toplevel.txt":        "root art\n",
		".git/config":         "[core]\n",
		"readme.md":           "# not art\n",       // should be skipped
		"image.png":           "\x89PNG\r\n\x1a\n", // should be skipped
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func TestCatalog_Refresh(t *testing.T) {
	dir := setupTestDir(t)
	log := slog.Default()
	cat := NewCatalog(dir, log)

	if err := cat.Refresh(); err != nil {
		t.Fatalf("Refresh() error: %v", err)
	}

	// Should index: dragon.txt, unicorn.ans, laugh.txt, joke.asc, toplevel.txt
	// Should NOT index: .git/config, readme.md, image.png
	if got := cat.Count(); got != 5 {
		t.Errorf("Count() = %d, want 5", got)
	}
}

func TestCatalog_FindByName(t *testing.T) {
	dir := setupTestDir(t)
	log := slog.Default()
	cat := NewCatalog(dir, log)
	cat.Refresh()

	tests := []struct {
		name      string
		wantCount int
	}{
		{"dragon", 1},
		{"Dragon", 1}, // case-insensitive
		{"DRAGON", 1},
		{"unicorn", 1},
		{"nonexistent", 0},
		{"toplevel", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := cat.FindByName(tt.name)
			if len(entries) != tt.wantCount {
				t.Errorf("FindByName(%q) returned %d entries, want %d", tt.name, len(entries), tt.wantCount)
			}
		})
	}
}

func TestCatalog_FindByName_Category(t *testing.T) {
	dir := setupTestDir(t)
	log := slog.Default()
	cat := NewCatalog(dir, log)
	cat.Refresh()

	entries := cat.FindByName("dragon")
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Category != "animals" {
		t.Errorf("dragon category = %q, want %q", entries[0].Category, "animals")
	}

	entries = cat.FindByName("toplevel")
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Category != "" {
		t.Errorf("toplevel category = %q, want empty", entries[0].Category)
	}
}

func TestCatalog_Search(t *testing.T) {
	dir := setupTestDir(t)
	log := slog.Default()
	cat := NewCatalog(dir, log)
	cat.Refresh()

	tests := []struct {
		query     string
		wantMin   int // minimum expected results
		wantNames []string
	}{
		{"dragon", 1, []string{"dragon"}},
		{"ag", 1, []string{"dragon"}}, // substring match
		{"uni", 1, []string{"unicorn"}},
		{"animals", 2, nil}, // matches category
		{"xyz", 0, nil},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			results := cat.Search(tt.query)
			if len(results) < tt.wantMin {
				t.Errorf("Search(%q) returned %d results, want at least %d", tt.query, len(results), tt.wantMin)
			}
			if tt.wantNames != nil {
				for _, want := range tt.wantNames {
					found := false
					for _, r := range results {
						if r.Name == want {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("Search(%q) missing expected result %q", tt.query, want)
					}
				}
			}
		})
	}
}

func TestCatalog_ListCategories(t *testing.T) {
	dir := setupTestDir(t)
	log := slog.Default()
	cat := NewCatalog(dir, log)
	cat.Refresh()

	cats := cat.ListCategories()

	// Should have "animals" and "funny"
	if len(cats) != 2 {
		t.Fatalf("ListCategories() returned %d, want 2: %v", len(cats), cats)
	}
	// Should be sorted
	if cats[0] != "animals" || cats[1] != "funny" {
		t.Errorf("ListCategories() = %v, want [animals funny]", cats)
	}
}

func TestCatalog_ListByCategory(t *testing.T) {
	dir := setupTestDir(t)
	log := slog.Default()
	cat := NewCatalog(dir, log)
	cat.Refresh()

	entries := cat.ListByCategory("animals")
	if len(entries) != 2 {
		t.Errorf("ListByCategory(animals) returned %d entries, want 2", len(entries))
	}

	entries = cat.ListByCategory("Animals") // case-insensitive
	if len(entries) != 2 {
		t.Errorf("ListByCategory(Animals) returned %d entries, want 2", len(entries))
	}

	entries = cat.ListByCategory("nonexistent")
	if len(entries) != 0 {
		t.Errorf("ListByCategory(nonexistent) returned %d entries, want 0", len(entries))
	}
}

func TestCatalog_SkipsHiddenDirs(t *testing.T) {
	dir := setupTestDir(t)
	log := slog.Default()
	cat := NewCatalog(dir, log)
	cat.Refresh()

	// .git/config should not be indexed
	results := cat.Search("config")
	if len(results) != 0 {
		t.Errorf("Search('config') returned %d results, want 0 (should skip .git)", len(results))
	}
}

func TestCatalog_SkipsNonArtFiles(t *testing.T) {
	dir := setupTestDir(t)
	log := slog.Default()
	cat := NewCatalog(dir, log)
	cat.Refresh()

	// readme.md and image.png should not be indexed
	results := cat.Search("readme")
	if len(results) != 0 {
		t.Errorf("Search('readme') returned %d results, want 0", len(results))
	}

	results = cat.Search("image")
	if len(results) != 0 {
		t.Errorf("Search('image') returned %d results, want 0", len(results))
	}
}

func TestIsArtFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"dragon.txt", true},
		{"dragon.ans", true},
		{"dragon.asc", true},
		{"dragon.nfo", true},
		{"dragon.diz", true},
		{"dragon", true},      // no extension
		{"dragon.md", false},  // markdown
		{"dragon.png", false}, // image
		{"dragon.go", false},  // code
		{".hidden", false},    // hidden file
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isArtFile(tt.name); got != tt.want {
				t.Errorf("isArtFile(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
