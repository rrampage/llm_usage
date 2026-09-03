package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDetermineDateBounds(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 9, 3, 14, 0, 0, 0, loc)

	t.Run("daily default 3 months", func(t *testing.T) {
		since, until, err := determineDateBounds("", "", false, "daily", loc, now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		expectedSince := time.Date(2026, 6, 3, 0, 0, 0, 0, loc)
		if !since.Equal(expectedSince) {
			t.Fatalf("got since=%v, want %v", since, expectedSince)
		}
		if !until.IsZero() {
			t.Fatalf("got until=%v, want zero", until)
		}
	})

	t.Run("daily with --all disables default 3 months", func(t *testing.T) {
		since, until, err := determineDateBounds("", "", true, "daily", loc, now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !since.IsZero() {
			t.Fatalf("got since=%v, want zero", since)
		}
		if !until.IsZero() {
			t.Fatalf("got until=%v, want zero", until)
		}
	})

	t.Run("daily with explicit --since", func(t *testing.T) {
		since, until, err := determineDateBounds("2026-08-01", "", false, "daily", loc, now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		expectedSince := time.Date(2026, 8, 1, 0, 0, 0, 0, loc)
		if !since.Equal(expectedSince) {
			t.Fatalf("got since=%v, want %v", since, expectedSince)
		}
		if !until.IsZero() {
			t.Fatalf("got until=%v, want zero", until)
		}
	})

	t.Run("monthly without --since remains unbounded", func(t *testing.T) {
		since, until, err := determineDateBounds("", "", false, "monthly", loc, now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !since.IsZero() {
			t.Fatalf("got since=%v, want zero", since)
		}
		if !until.IsZero() {
			t.Fatalf("got until=%v, want zero", until)
		}
	})

	t.Run("weekly without --since remains unbounded", func(t *testing.T) {
		since, until, err := determineDateBounds("", "", false, "weekly", loc, now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !since.IsZero() {
			t.Fatalf("got since=%v, want zero", since)
		}
		if !until.IsZero() {
			t.Fatalf("got until=%v, want zero", until)
		}
	})

	t.Run("daily with until before 3-month cutoff leaves since unbounded", func(t *testing.T) {
		since, until, err := determineDateBounds("", "2026-04-01", false, "daily", loc, now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !since.IsZero() {
			t.Fatalf("got since=%v, want zero", since)
		}
		expectedUntil := time.Date(2026, 4, 1, 23, 59, 59, int(time.Second-time.Nanosecond), loc)
		if !until.Equal(expectedUntil) {
			t.Fatalf("got until=%v, want %v", until, expectedUntil)
		}
	})

	t.Run("since after until returns error", func(t *testing.T) {
		_, _, err := determineDateBounds("2026-09-05", "2026-09-01", false, "daily", loc, now)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestShouldSkipDateDir(t *testing.T) {
	loc := time.UTC
	since := time.Date(2026, 6, 3, 0, 0, 0, 0, loc)
	until := time.Date(2026, 8, 15, 23, 59, 59, 0, loc)
	root := "/sessions"

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"year before since", "/sessions/2025", true},
		{"current year", "/sessions/2026", false},
		{"year after until", "/sessions/2027", true},
		{"month before since", "/sessions/2026/05", true},
		{"month containing since", "/sessions/2026/06", false},
		{"month before until", "/sessions/2026/07", false},
		{"month after until", "/sessions/2026/09", true},
		{"day before since", "/sessions/2026/06/02", true},
		{"day on since", "/sessions/2026/06/03", false},
		{"day within range", "/sessions/2026/07/10", false},
		{"day after until", "/sessions/2026/08/16", true},
		{"non-date directory", "/sessions/my-project", false},
		{"root itself", "/sessions", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldSkipDateDir(root, tc.path, since, until)
			if got != tc.want {
				t.Errorf("shouldSkipDateDir(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestWalkJSONLPruning(t *testing.T) {
	tmp := t.TempDir()
	loc := time.UTC

	oldDir := filepath.Join(tmp, "2026", "04", "01")
	newDir := filepath.Join(tmp, "2026", "07", "01")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}

	oldFile := filepath.Join(oldDir, "session1.jsonl")
	newFile := filepath.Join(newDir, "session2.jsonl")

	if err := os.WriteFile(oldFile, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newFile, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	since := time.Date(2026, 6, 3, 0, 0, 0, 0, loc)

	// Bounded walk with since
	boundedFiles, err := walkJSONL(tmp, since, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(boundedFiles) != 1 || boundedFiles[0] != newFile {
		t.Fatalf("got boundedFiles=%v, want [%s]", boundedFiles, newFile)
	}

	// Unbounded walk
	allFiles, err := walkJSONL(tmp, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(allFiles) != 2 {
		t.Fatalf("got %d files, want 2", len(allFiles))
	}
}
