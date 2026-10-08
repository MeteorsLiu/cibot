package toc

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
)

// File holds a toc path and serializes Add calls on the same instance.
// A File must not be copied after first use. Separate instances for the same path
// require synchronization by the caller.
type File struct {
	name string
	// mu covers the complete lookup and append, so concurrent calls cannot both add a record.
	mu sync.Mutex
}

// Parse initializes a toc file without reading or validating its contents.
func Parse(file string) (File, error) {
	return File{name: file}, nil
}

// Add appends "full proj" unless the file contains that byte sequence.
// It returns true when appended, or false for a substring match.
// Source and project are used as supplied.
func (f *File) Add(proj, full string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	found, needsNewline, err := f.check(proj, full)
	if err != nil || found {
		return false, err
	}

	record := full + " " + proj + "\n"
	if needsNewline {
		record = "\n" + record
	}
	writer, err := os.OpenFile(f.name, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return false, err
	}
	_, writeErr := writer.WriteString(record)
	closeErr := writer.Close()
	if writeErr != nil {
		return false, writeErr
	}
	return closeErr == nil, closeErr
}

// check searches the file with a local read buffer.
// The second result reports whether the last record needs a line terminator.
func (f *File) check(proj, full string) (found, needsNewline bool, err error) {
	var buffer [64 * 1024]byte
	if len(full)+len(proj)+2 > len(buffer) {
		return false, false, fmt.Errorf("mapping exceeds %d-byte buffer", len(buffer))
	}
	record := []byte(full + " " + proj)

	reader, err := os.Open(f.name)
	if err != nil {
		return false, false, err
	}
	defer reader.Close()

	pending := 0
	for {
		n, err := reader.Read(buffer[pending:])
		if err != nil && err != io.EOF {
			return false, false, fmt.Errorf("scan %s: %w", f.name, err)
		}
		data := buffer[:pending+n]
		if bytes.Contains(data, record) {
			return true, false, nil
		}
		if n > 0 {
			needsNewline = data[len(data)-1] != '\n'
		}
		if err == io.EOF {
			return false, needsNewline, nil
		}
		// A match can start in this read and finish in the next one.
		pending = min(len(record)-1, len(data))
		copy(buffer[:], data[len(data)-pending:])
	}
}
