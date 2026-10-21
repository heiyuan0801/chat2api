// Package filepersist provides the atomic JSON and local process locks shared
// by file-backed Image Tasks and Image Assets. It never redirects files to SQL.
package filepersist

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ReadJSON reads a primary file, then its compatible .bak when the primary is
// absent or unreadable. Numbers retain their JSON spelling and precision.
func ReadJSON(path string, out any) error {
	var errs []error
	for _, candidate := range []string{path, path + ".bak"} {
		data, err := os.ReadFile(candidate)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		var validated json.RawMessage
		if err = decoder.Decode(&validated); err == nil {
			trimmed := bytes.TrimSpace(validated)
			if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
				errs = append(errs, fmt.Errorf("invalid JSON snapshot type in %s", filepath.Base(candidate)))
				continue
			}
			var extra any
			if err = decoder.Decode(&extra); err == io.EOF {
				decoder = json.NewDecoder(bytes.NewReader(validated))
				decoder.UseNumber()
				if err = decoder.Decode(out); err == nil {
					return nil
				}
			}
			if err == nil {
				err = errors.New("unexpected trailing JSON value")
			}
		}
		errs = append(errs, fmt.Errorf("invalid JSON file %s: %w", filepath.Base(candidate), err))
	}
	return errors.Join(errs...)
}

func atomicWrite(path string, payload []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(payload)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}

// WriteJSON commits the primary by atomic replacement. The backup contains the
// same committed snapshot, matching Python's JSON helper. Failure to refresh a
// backup does not roll back an already committed primary.
func WriteJSON(path string, value any) error {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	if err := atomicWrite(path, buf.Bytes()); err != nil {
		return err
	}
	_ = atomicWrite(path+".bak", buf.Bytes())
	return nil
}
