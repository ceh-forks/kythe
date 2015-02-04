/*
 * Copyright 2014 Google Inc. All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package indexpack provides an interface to a collection of compilation units
// stored in an "index pack" directory structure.  The index pack format is
// defined in kythe-index-pack.txt.
//
// Example usage, writing:
//   pack, err := indexpack.Create(ctx, "path/to/some/directory")
//   if err != nil {
//     log.Exit(err)
//   }
//   for _, cu := range fetchUnits() {
//     if _, err := pack.WriteUnit(ctx, "kythe", cu); err != nil {
//       log.Error(err)
//       continue
//     }
//     for _, input := range cu.RequiredInput {
//       data, err := readFile(input)
//       if err != nil {
//         log.Error(err)
//         continue
//       }
//       if err := pack.WriteFile(ctx, data); err != nil {
//         log.Error(err)
//       }
//     }
//   }
//
// Example usage, reading:
//   pack, err := indexpack.Open(ctx, "some/dir/path", indexpack.UnitType((*cpb.CompilationUnit)(nil)))
//   if err != nil {
//     log.Exit(err)
//   }
//
//   // The value passed to the callback will have the concrete type of
//   // the third parameter to Open (or Create).
//   err := pack.ReadUnits(ctx, "kythe", func(cu interface{}) error {
//     for _, input := range cu.(*cpb.CompilationUnit).RequiredInput {
//       bits, err := pack.ReadFile(ctx, input.GetDigest())
//       if err != nil {
//         return err
//       }
//       processData(bits)
//     }
//     return doSomethingUseful(cu)
//   })
//   if err != nil {
//     log.Exit(err)
//   }
package indexpack

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"

	"kythe/go/platform/analysis"

	"code.google.com/p/go-uuid/uuid"
	"golang.org/x/net/context"
)

const (
	dataDir    = "files"
	unitDir    = "units"
	dataSuffix = ".data" // Filename suffix for a file-data file
	unitSuffix = ".unit" // Filename suffix for a compilation unit file
	newSuffix  = ".new"  // Filename suffix for a temporary file used during writing
)

// A unitWrapper captures the top-level JSON structure for a compilation unit
// stored in an index pack.
type unitWrapper struct {
	Format  string          `json:"format"`
	Content json.RawMessage `json:"content"`
}

type packFetcher struct {
	context.Context
	*Archive
}

// Fetch implements analysis.Fetcher by fetching the digest from the index
// pack.  This implementation ignores the path, since there is no direct
// mapping for paths in an index pack.
func (p packFetcher) Fetch(path, digest string) ([]byte, error) {
	return p.Archive.ReadFile(p.Context, digest)
}

// Fetcher returns an analysis.Fetcher that reads file contents from a.
func (a *Archive) Fetcher(ctx context.Context) analysis.Fetcher { return packFetcher{ctx, a} }

// Archive represents an index pack directory.
type Archive struct {
	root     string       // The root path of the index pack
	unitType reflect.Type // The concrete value type for the ReadUnits callback
	fs       VFS          // Filesystem implementation used for file access
	closer   io.Closer    // If non-nil, this will be called at close
}

// VFS is the interface consumed by the Archive to manipulate the filesystem
// for index pack operations.
type VFS interface {
	// Stat returns file status information for path, as os.Stat.
	Stat(ctx context.Context, path string) (os.FileInfo, error)

	// MkdirAll recursively creates the specified directory path with the given
	// permissions, as os.MkdirAll.
	MkdirAll(ctx context.Context, path string, mode os.FileMode) error

	// Open opens an existing file for reading, as os.Open.
	Open(ctx context.Context, path string) (io.ReadCloser, error)

	// Create creates a new file for writing, as os.Create.
	Create(ctx context.Context, path string) (io.WriteCloser, error)

	// Rename renames oldPath to newPath, as os.Rename, overwriting newPath if
	// it exists.
	Rename(ctx context.Context, oldPath, newPath string) error

	// Remove deletes the file specified by path, as os.Remove.
	Remove(ctx context.Context, path string) error

	// Glob returns all the paths matching the specified glob pattern, as
	// filepath.Glob.
	Glob(ctx context.Context, glob string) ([]string, error)
}

// An Option is a configurable setting for an Archive.
type Option func(*Archive) error

// UnitType returns an Option that sets the concrete type used to unmarshal
// compilation units in the ReadUnits method.  If t != nil, the interface value
// passed to the callback will be a pointer to the type of t.
func UnitType(t interface{}) Option {
	return func(a *Archive) error {
		a.unitType = reflect.TypeOf(t)
		if a.unitType != nil && a.unitType.Kind() == reflect.Ptr {
			a.unitType = a.unitType.Elem()
		}
		return nil
	}
}

// FS returns an Option that sets the filesystem interface used to implement
// the index pack.
func FS(fs VFS) Option {
	return func(a *Archive) error {
		if fs == nil {
			return errors.New("invalid VFS")
		}
		a.fs = fs
		return nil
	}
}

// Create creates a new empty index pack at the specified path. It is an error
// if the path already exists.
//
// If unitType != nil, the interface value passed to the callback of ReadUnits
// will be a pointer to its type.
func Create(ctx context.Context, path string, opts ...Option) (*Archive, error) {
	a := &Archive{root: path, fs: localFS{}}
	for _, opt := range opts {
		if err := opt(a); err != nil {
			return nil, err
		}
	}

	if _, err := a.fs.Stat(ctx, path); err == nil {
		return nil, fmt.Errorf("path %q already exists", path)
	}
	if err := a.fs.MkdirAll(ctx, path, 0755); err != nil {
		return nil, err
	}
	if err := a.fs.MkdirAll(ctx, filepath.Join(path, unitDir), 0755); err != nil {
		return nil, err
	}
	if err := a.fs.MkdirAll(ctx, filepath.Join(path, dataDir), 0755); err != nil {
		return nil, err
	}
	return a, nil
}

// Open returns a handle for an existing valid index pack at the specified
// path.  It is an error if path does not exist, or does not have the correct
// format.
//
// If unitType != nil, the interface value passed to the callback of ReadUnits
// will be a pointer to its type.
func Open(ctx context.Context, path string, opts ...Option) (*Archive, error) {
	a := &Archive{root: path, fs: localFS{}}
	for _, opt := range opts {
		if err := opt(a); err != nil {
			return nil, err
		}
	}

	fi, err := a.fs.Stat(ctx, path)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("path %q is not a directory", path)
	}
	if fi, err := a.fs.Stat(ctx, filepath.Join(path, unitDir)); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("path %q is missing a units subdirectory", path)
	}
	if fi, err := a.fs.Stat(ctx, filepath.Join(path, dataDir)); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("path %q is missing a files subdirectory", path)
	}
	return a, nil
}

func (a *Archive) readFile(ctx context.Context, dir, name string) ([]byte, error) {
	f, err := a.fs.Open(ctx, filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return nil, err
	}

	return ioutil.ReadAll(gz)
}

// CreateOrOpen opens an existing index pack with the given parameters, if one
// exists; or if not, then attempts to create one.
func CreateOrOpen(ctx context.Context, path string, opts ...Option) (*Archive, error) {
	if a, err := Open(ctx, path, opts...); err == nil {
		return a, nil
	}
	return Create(ctx, path, opts...)
}

func (a *Archive) writeFile(ctx context.Context, dir, name string, data []byte) error {
	tmp := filepath.Join(dir, uuid.New()) + newSuffix
	f, err := a.fs.Create(ctx, tmp)
	if err != nil {
		return err
	}
	// When this function is called, the temp file is garbage; we make a good
	// faith effort to close it and clean it up, but we don't care if it fails.
	cleanup := func() error {
		err := f.Close()
		a.fs.Remove(ctx, tmp)
		return err
	}
	gz := gzip.NewWriter(f)
	if _, err := gz.Write(data); err != nil {
		return cleanup()
	}
	if err := gz.Flush(); err != nil {
		return cleanup()
	}
	if err := gz.Close(); err != nil {
		return cleanup()
	}
	if err := f.Close(); err != nil {
		a.fs.Remove(ctx, tmp)
		return err
	}
	return a.fs.Rename(ctx, tmp, filepath.Join(dir, name))
}

// ReadUnits calls f with each of the compilation units stored in the units
// subdirectory of the index pack whose format key equals formatKey.  The
// concrete type of the value passed to f will be the same as the concrete type
// of the unitType argument that was passed to Open or Create.
//
// If f returns a non-nil error, no further compilations are read and the error
// is propagated back to the caller of ReadUnits.
func (a *Archive) ReadUnits(ctx context.Context, formatKey string, f func(interface{}) error) error {
	units := filepath.Join(a.root, unitDir)
	fss, err := a.fs.Glob(ctx, filepath.Join(units, "*"+unitSuffix))
	if err != nil {
		return err
	}
	for _, fs := range fss {
		base := filepath.Base(fs)
		data, err := a.readFile(ctx, units, base)
		if err != nil {
			return err
		}

		// Parse the unit wrapper, {"format": "kythe", "content": ...}
		var unit unitWrapper
		if err := json.Unmarshal(data, &unit); err != nil {
			return fmt.Errorf("error parsing unit: %v", err)
		}
		if unit.Format != formatKey {
			continue // Format does not match; skip this one
		}
		if len(unit.Content) == 0 {
			return errors.New("invalid compilation unit")
		}

		// Parse the content into the receiver's type.
		cu := reflect.New(a.unitType).Interface()
		if err := json.Unmarshal(unit.Content, cu); err != nil {
			return fmt.Errorf("error parsing content: %v", err)
		}
		if err := f(cu); err != nil {
			return err
		}
	}
	return nil
}

// ReadFile reads and returns the file contents corresponding to the given
// hex-encoded SHA-256 digest.
func (a *Archive) ReadFile(ctx context.Context, digest string) ([]byte, error) {
	return a.readFile(ctx, filepath.Join(a.root, dataDir), digest+dataSuffix)
}

// WriteUnit writes the specified compilation unit to the units/ subdirectory
// of the index pack, using the specified format key.  Returns the resulting
// filename, whether or not there is an error in writing the file, as long as
// marshaling succeeded.
func (a *Archive) WriteUnit(ctx context.Context, formatKey string, cu interface{}) (string, error) {
	// Convert the compilation unit into JSON.
	content, err := json.Marshal(cu)
	if err != nil {
		return "", fmt.Errorf("error marshaling content: %v", err)
	}

	// Pack the resulting message into a compilation wrapper for output.
	data, err := json.Marshal(&unitWrapper{
		Format:  formatKey,
		Content: content,
	})
	if err != nil {
		return "", fmt.Errorf("error marshaling unit: %v", err)
	}
	name := hexDigest(data) + unitSuffix
	return name, a.writeFile(ctx, filepath.Join(a.root, unitDir), name, data)
}

// WriteFile writes the specified file contents to the files/ subdirectory of
// the index pack.  Returns the resulting filename, whether or not there is an
// error in writing the file.
func (a *Archive) WriteFile(ctx context.Context, data []byte) (string, error) {
	name := hexDigest(data) + dataSuffix
	return name, a.writeFile(ctx, filepath.Join(a.root, dataDir), name, data)
}

// Root returns the root path of the archive.
func (a *Archive) Root() string { return a.root }

// Close releases any resources held by the archive.  If the archive was
// created with the default VFS, this is a no-op and is optional.
func (a *Archive) Close() error {
	if a.closer != nil {
		return a.closer.Close()
	}
	return nil
}

func hexDigest(data []byte) string {
	h := sha256.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
