package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func decodeJSONUseNumber(b []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(dst)
}

func jsonU64(v any) (uint64, bool) {
	switch x := v.(type) {
	case json.Number:
		if n, err := strconv.ParseUint(x.String(), 10, 64); err == nil {
			return n, true
		}
		if f, err := strconv.ParseFloat(x.String(), 64); err == nil && f >= 0 && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return uint64(f), true
		}
	case float64:
		if x >= 0 && !math.IsNaN(x) && !math.IsInf(x, 0) {
			return uint64(x), true
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil && f >= 0 && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return uint64(f), true
		}
	}
	return 0, false
}

func firstJSONU64(record map[string]any, keys ...string) uint64 {
	for _, key := range keys {
		if v, ok := jsonU64(record[key]); ok {
			return v
		}
	}
	return 0
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func firstMapString(record map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := record[key].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func firstTimestamp(record map[string]any, keys ...string) time.Time {
	for _, key := range keys {
		if s, ok := record[key].(string); ok {
			if t, ok := parseTimestamp(s); ok {
				return t
			}
		}
	}
	return time.Time{}
}

func firstTimestampOr(record map[string]any, fallback time.Time, keys ...string) time.Time {
	if t := firstTimestamp(record, keys...); !t.IsZero() {
		return t
	}
	return fallback
}

func fileModifiedTime(path string) time.Time {
	if st, err := os.Stat(path); err == nil {
		if t := st.ModTime(); !t.IsZero() {
			return t
		}
	}
	return time.Unix(0, 0).UTC()
}

func shouldSkipDateDir(root, path string, since, until time.Time) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	loc := time.Local
	if !since.IsZero() {
		loc = since.Location()
	} else if !until.IsZero() {
		loc = until.Location()
	}
	switch len(parts) {
	case 1:
		if len(parts[0]) == 4 {
			if y, err := strconv.Atoi(parts[0]); err == nil {
				if !since.IsZero() {
					endOfYear := time.Date(y+1, 1, 1, 0, 0, 0, 0, loc)
					if endOfYear.Before(since) || endOfYear.Equal(since) {
						return true
					}
				}
				if !until.IsZero() {
					startOfYear := time.Date(y, 1, 1, 0, 0, 0, 0, loc)
					if startOfYear.After(until) {
						return true
					}
				}
			}
		}
	case 2:
		if len(parts[0]) == 4 && len(parts[1]) == 2 {
			if y, err := strconv.Atoi(parts[0]); err == nil {
				if m, err := strconv.Atoi(parts[1]); err == nil && m >= 1 && m <= 12 {
					if !since.IsZero() {
						endOfMonth := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
						if endOfMonth.Before(since) || endOfMonth.Equal(since) {
							return true
						}
					}
					if !until.IsZero() {
						startOfMonth := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, loc)
						if startOfMonth.After(until) {
							return true
						}
					}
				}
			}
		}
	case 3:
		if len(parts[0]) == 4 && len(parts[1]) == 2 && len(parts[2]) == 2 {
			if y, err := strconv.Atoi(parts[0]); err == nil {
				if m, err := strconv.Atoi(parts[1]); err == nil && m >= 1 && m <= 12 {
					if d, err := strconv.Atoi(parts[2]); err == nil && d >= 1 && d <= 31 {
						if !since.IsZero() {
							endOfDay := time.Date(y, time.Month(m), d, 0, 0, 0, 0, loc).AddDate(0, 0, 1)
							if endOfDay.Before(since) || endOfDay.Equal(since) {
								return true
							}
						}
						if !until.IsZero() {
							startOfDay := time.Date(y, time.Month(m), d, 0, 0, 0, 0, loc)
							if startOfDay.After(until) {
								return true
							}
						}
					}
				}
			}
		}
	}
	return false
}

func walkExtensions(root string, since, until time.Time, extensions ...string) ([]string, error) {
	wanted := map[string]bool{}
	for _, ext := range extensions {
		wanted[strings.ToLower(ext)] = true
	}
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if (!since.IsZero() || !until.IsZero()) && shouldSkipDateDir(root, path, since, until) {
				return filepath.SkipDir
			}
			return nil
		}
		if wanted[strings.ToLower(filepath.Ext(d.Name()))] {
			if !since.IsZero() {
				info, err := d.Info()
				if err == nil && info.ModTime().Before(since) {
					return nil
				}
			}
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func minU64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
