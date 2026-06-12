package utils

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	pTrue = true
	PTrue = &pTrue

	pFalse = false
	PFalse = &pFalse
)

// IsEmptyString returns true if the string is empty or contains only whitespace.
func IsEmptyString(s string) bool {
	return len(strings.TrimSpace(s)) == 0
}

// CheckPath validates and normalizes the provided file path.
// Returns an error if the path is empty, otherwise returns the cleaned path.
func CheckPath(filePath string) (string, error) {
	if IsEmptyString(filePath) {
		return "", fmt.Errorf("file path cannot be empty")
	}
	return filepath.Clean(filePath), nil
}

// DeepCopy performs a deep copy of src into dst via JSON serialization.
func DeepCopy(dst interface{}, src interface{}) error {
	payload, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, dst)
}

// FileExists reports whether a file exists at the given path.
// Returns (true, FileInfo, nil) if it exists, (false, nil, nil) if not found,
// or (false, nil, err) on unexpected error.
func FileExists(path string) (bool, os.FileInfo, error) {
	if IsEmptyString(path) {
		return false, nil, fmt.Errorf("path is empty")
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil, nil
		}
		return false, nil, err
	}
	return true, fileInfo, nil
}

// Truncate clears the contents of a file by truncating it to zero length.
func Truncate(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0666)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			fmt.Printf("Error closing file: %v\n", cerr)
		}
	}()

	if err = file.Truncate(0); err != nil {
		return err
	}
	_, err = file.Seek(0, 0)
	return err
}

// Remove deletes the element at index i from a slice of channels, preserving order.
func Remove(ss []chan interface{}, i int) []chan interface{} {
	copy(ss[i:], ss[i+1:])
	ss[len(ss)-1] = nil
	ss = ss[:len(ss)-1]
	return ss
}

// Contains reports whether item exists in slice.
func Contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// ExcludeFiles reports whether fileName exactly matches any entry in exclusions.
func ExcludeFiles(fileName string, exclusions []string) bool {
	for _, ex := range exclusions {
		if fileName == ex {
			return true
		}
	}
	return false
}
