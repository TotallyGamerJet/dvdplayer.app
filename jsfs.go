package main

import (
	"errors"
	"io"
	"io/fs"
	"path"
	"strings"
	"syscall/js"
	"time"
)

type FS struct {
	root js.Value // FileSystemDirectoryHandle
}

func NewFS(root js.Value) *FS {
	return &FS{root: root}
}

func (f *FS) Open(name string) (fs.File, error) {
	name = path.Clean(name)
	if name == "." {
		return &Dir{handle: f.root, name: "."}, nil
	}

	parts := strings.Split(name, "/")
	handle := f.root

	// Traverse directories
	for i := 0; i < len(parts)-1; i++ {
		dirHandle, err := await(handle.Call("getDirectoryHandle", parts[i]))
		if err != nil {
			return nil, err
		}
		handle = dirHandle
	}

	last := parts[len(parts)-1]

	// Try file first
	fileHandle, err := await(handle.Call("getFileHandle", last))
	if err == nil {
		fileObj, err := await(fileHandle.Call("getFile"))
		if err != nil {
			return nil, err
		}

		return &File{
			name:    last,
			fileObj: fileObj,
			size:    int64(fileObj.Get("size").Float()),
		}, nil
	}

	// Try directory
	dirHandle, err := await(handle.Call("getDirectoryHandle", last))
	if err == nil {
		return &Dir{handle: dirHandle, name: last}, nil
	}

	return nil, fs.ErrNotExist
}

// ---------------- File ----------------

type File struct {
	name    string
	fileObj js.Value // JS File object, used for lazy loading
	size    int64
	data    []byte
	loaded  bool
	pos     int64
}

func (f *File) Stat() (fs.FileInfo, error) {
	return fileInfo{
		name: f.name,
		size: f.size,
		mode: 0444,
	}, nil
}

func (f *File) Read(p []byte) (int, error) {
	// Lazy load: read file data on first Read call
	if !f.loaded {
		data, err := readFile(f.fileObj)
		if err != nil {
			return 0, err
		}
		f.data = data
		f.loaded = true
	}

	if f.pos >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.pos:])
	f.pos += int64(n)
	return n, nil
}

func (f *File) Close() error {
	return nil
}

// ---------------- Directory ----------------

type Dir struct {
	handle  js.Value
	name    string
	entries []fs.DirEntry
	read    bool
}

func (d *Dir) ReadDir(n int) ([]fs.DirEntry, error) {
	if !d.read {
		entries, err := readDirEntries(d.handle)
		if err != nil {
			return nil, err
		}
		d.entries = entries
		d.read = true
	}

	if len(d.entries) == 0 {
		return nil, io.EOF
	}

	if n <= 0 || n > len(d.entries) {
		n = len(d.entries)
	}

	out := d.entries[:n]
	d.entries = d.entries[n:]
	return out, nil
}

func (d *Dir) Stat() (fs.FileInfo, error) {
	return fileInfo{
		name: d.name,
		mode: fs.ModeDir | 0555,
	}, nil
}

func (d *Dir) Read([]byte) (int, error) {
	return 0, errors.New("cannot read directory")
}

func (d *Dir) Close() error {
	return nil
}

// ---------------- DirEntry ----------------

type dirEntry struct {
	name  string
	isDir bool
}

func (d dirEntry) Name() string { return d.name }
func (d dirEntry) IsDir() bool  { return d.isDir }
func (d dirEntry) Type() fs.FileMode {
	if d.isDir {
		return fs.ModeDir
	}
	return 0
}
func (d dirEntry) Info() (fs.FileInfo, error) {
	return fileInfo{
		name: d.name,
		mode: d.Type(),
	}, nil
}

// ---------------- FileInfo ----------------

type fileInfo struct {
	name string
	size int64
	mode fs.FileMode
}

func (f fileInfo) Name() string       { return f.name }
func (f fileInfo) Size() int64        { return f.size }
func (f fileInfo) Mode() fs.FileMode  { return f.mode }
func (f fileInfo) ModTime() time.Time { return time.Time{} }
func (f fileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fileInfo) Sys() any           { return nil }

// ---------------- JS Helpers ----------------

// await blocks on a JS Promise
func await(p js.Value, args ...any) (js.Value, error) {
	ch := make(chan struct{})
	var result js.Value
	var err error

	then := js.FuncOf(func(this js.Value, args []js.Value) any {
		result = args[0]
		close(ch)
		return nil
	})

	catch := js.FuncOf(func(this js.Value, args []js.Value) any {
		err = errors.New(args[0].String())
		close(ch)
		return nil
	})

	p.Call("then", then).Call("catch", catch)
	<-ch

	then.Release()
	catch.Release()

	return result, err
}

func readFile(file js.Value) ([]byte, error) {
	buf, err := await(file.Call("arrayBuffer"))
	if err != nil {
		return nil, err
	}

	uint8Array := js.Global().Get("Uint8Array").New(buf)
	data := make([]byte, uint8Array.Length())
	js.CopyBytesToGo(data, uint8Array)
	return data, nil
}

func readDirEntries(handle js.Value) ([]fs.DirEntry, error) {
	iter := handle.Call("entries")

	var entries []fs.DirEntry

	for {
		next, err := await(iter.Call("next"))
		if err != nil {
			return nil, err
		}
		if next.Get("done").Bool() {
			break
		}

		value := next.Get("value")
		name := value.Index(0).String()
		child := value.Index(1)

		kind := child.Get("kind").String()
		entries = append(entries, dirEntry{
			name:  name,
			isDir: kind == "directory",
		})
	}

	return entries, nil
}
