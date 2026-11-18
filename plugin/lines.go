package plugin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadLines appends to items the JSON Lines of T in an answer's files in the work directory.
func ReadLines[T any](work string, files []string, items []T) ([]T, error) {
	for _, f := range files {
		if filepath.IsAbs(f) || strings.Contains(f, "..") {
			return nil, fmt.Errorf("%w: answer file %q is outside the work directory", ErrFailed, f)
		}

		data, err := os.ReadFile(filepath.Join(work, f)) //nolint:gosec // a file inside the call's work directory
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrFailed, err)
		}

		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)

		for sc.Scan() {
			if len(bytes.TrimSpace(sc.Bytes())) == 0 {
				continue
			}

			var it T
			if err := json.Unmarshal(sc.Bytes(), &it); err != nil {
				return nil, fmt.Errorf("%w: %s: %w", ErrFailed, f, err)
			}

			items = append(items, it)
		}

		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrFailed, f, err)
		}
	}

	return items, nil
}
