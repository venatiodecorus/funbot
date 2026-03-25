package art

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// CatalogEntry represents a single art file in the catalog.
type CatalogEntry struct {
	Name     string // filename without extension (e.g., "dragon")
	Category string // parent directory name (e.g., "animals")
	Path     string // full filesystem path
	Lines    int    // number of lines in the file (0 if not yet counted)
}

// Catalog indexes and provides search over the art file repository.
type Catalog struct {
	basePath string
	log      *slog.Logger

	mu      sync.RWMutex
	entries []CatalogEntry
	byName  map[string][]CatalogEntry // lowercase name -> entries
	byCat   map[string][]CatalogEntry // lowercase category -> entries
}

// NewCatalog creates a new art catalog rooted at the given path.
func NewCatalog(basePath string, log *slog.Logger) *Catalog {
	return &Catalog{
		basePath: basePath,
		log:      log.With("component", "art-catalog"),
		byName:   make(map[string][]CatalogEntry),
		byCat:    make(map[string][]CatalogEntry),
	}
}

// Refresh scans the art repository directory and rebuilds the index.
// This should be called after initial clone and after each pull.
func (c *Catalog) Refresh() error {
	c.log.Info("refreshing art catalog", "path", c.basePath)

	var entries []CatalogEntry
	byName := make(map[string][]CatalogEntry)
	byCat := make(map[string][]CatalogEntry)

	err := filepath.Walk(c.basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}

		// Skip hidden directories (like .git)
		if info.IsDir() && strings.HasPrefix(info.Name(), ".") {
			return filepath.SkipDir
		}

		if info.IsDir() {
			return nil
		}

		// Only index text files that could be art
		if !isArtFile(info.Name()) {
			return nil
		}

		// Determine category from parent directory
		rel, err := filepath.Rel(c.basePath, path)
		if err != nil {
			return nil
		}

		category := ""
		dir := filepath.Dir(rel)
		if dir != "." {
			// Use the top-level directory as the category
			parts := strings.SplitN(dir, string(filepath.Separator), 2)
			category = parts[0]
		}

		name := strings.TrimSuffix(info.Name(), filepath.Ext(info.Name()))

		entry := CatalogEntry{
			Name:     name,
			Category: category,
			Path:     path,
		}

		entries = append(entries, entry)

		lowerName := strings.ToLower(name)
		byName[lowerName] = append(byName[lowerName], entry)

		if category != "" {
			lowerCat := strings.ToLower(category)
			byCat[lowerCat] = append(byCat[lowerCat], entry)
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("walking art directory: %w", err)
	}

	// Sort entries by name for consistent output
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Category != entries[j].Category {
			return entries[i].Category < entries[j].Category
		}
		return entries[i].Name < entries[j].Name
	})

	c.mu.Lock()
	c.entries = entries
	c.byName = byName
	c.byCat = byCat
	c.mu.Unlock()

	c.log.Info("art catalog refreshed", "total_files", len(entries), "categories", len(byCat))
	return nil
}

// FindByName returns all entries matching the exact name (case-insensitive).
func (c *Catalog) FindByName(name string) []CatalogEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.byName[strings.ToLower(name)]
}

// Search returns entries whose name contains the query string (case-insensitive).
func (c *Catalog) Search(query string) []CatalogEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()

	query = strings.ToLower(query)
	var results []CatalogEntry

	for _, entry := range c.entries {
		if strings.Contains(strings.ToLower(entry.Name), query) ||
			strings.Contains(strings.ToLower(entry.Category), query) {
			results = append(results, entry)
		}
	}

	return results
}

// ListCategories returns all known category names, sorted alphabetically.
func (c *Catalog) ListCategories() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	cats := make([]string, 0, len(c.byCat))
	for cat := range c.byCat {
		cats = append(cats, cat)
	}
	sort.Strings(cats)
	return cats
}

// ListByCategory returns all entries in a given category (case-insensitive).
func (c *Catalog) ListByCategory(category string) []CatalogEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.byCat[strings.ToLower(category)]
}

// Count returns the total number of indexed art files.
func (c *Catalog) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// isArtFile returns true if the filename is likely an art file.
// We accept common text file extensions and extensionless files.
func isArtFile(name string) bool {
	// Skip obvious non-art files
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, ".") {
		return false
	}

	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".txt", ".ans", ".asc", ".nfo", ".diz", "":
		return true
	default:
		return false
	}
}
