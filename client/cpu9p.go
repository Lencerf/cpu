// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hugelgupf/p9/p9"
	"golang.org/x/sys/unix"
)

// cpu9p is a p9.Attacher.
type cpu9p struct {
	p9.DefaultWalkGetAttr

	path string
	file *os.File

	// pendingXattr is the xattr-related operations that are going to be done
	// in a tread or twrite request.
	pendingXattr pendingXattr
}

// xattrOp is the xattr related operations, walk or create.
type xattrOp int

const (
	xattrNone   = 0
	xattrCreate = 1
	xattrWalk   = 2
)

type pendingXattr struct {
	// the pending xattr-related operation
	op xattrOp

	// name is the attribute.
	name string

	// size of the attribute value, represents the
	// length of the attribute value that is going to write to or read from a file.
	size uint64

	// flags associated with a txattrcreate message.
	// generally Linux setxattr(2) flags.
	flags uint32
}

// Attach implements p9.Attacher.Attach.
func (l *cpu9p) Attach() (p9.File, error) {
	return &cpu9p{path: l.path}, nil
}

var (
	_ p9.File     = &cpu9p{}
	_ p9.Attacher = &cpu9p{}
)

// info constructs a QID for this file.
func (l *cpu9p) info() (p9.QID, os.FileInfo, error) {
	var (
		qid p9.QID
		fi  os.FileInfo
		err error
	)

	// Stat the file.
	if l.file != nil {
		fi, err = l.file.Stat()
	} else {
		fi, err = os.Lstat(l.path)
	}
	if err != nil {
		//log.Printf("error stating %#v: %v", l, err)
		return qid, nil, err
	}

	// Construct the QID type.
	qid.Type = p9.ModeFromOS(fi.Mode()).QIDType()

	// Save the path from the Ino.
	qid.Path = fi.Sys().(*syscall.Stat_t).Ino
	return qid, fi, nil
}

func (l *cpu9p) XattrWalk(attr string) (p9.File, uint64, error) {
	emptyBuf := make([]byte, 0)
	var size int
	var err error
	if attr == "" {
		size, err = unix.Llistxattr(l.path, emptyBuf)
	} else {
		size, err = unix.Lgetxattr(l.path, attr, emptyBuf)
	}
	newFile := &cpu9p{
		path: l.path,
		pendingXattr: pendingXattr{
			op:   xattrWalk,
			name: attr,
			size: uint64(size),
		},
	}
	return newFile, uint64(size), err
}

func (l *cpu9p) XattrCreate(attr string, size uint64, flags uint32) error {
	l.pendingXattr.op = xattrCreate
	l.pendingXattr.name = attr
	l.pendingXattr.size = size
	l.pendingXattr.flags = flags
	return nil
}

// Walk implements p9.File.Walk.
func (l *cpu9p) Walk(names []string) ([]p9.QID, p9.File, error) {
	var qids []p9.QID
	last := &cpu9p{path: l.path}
	// If the names are empty we return info for l
	// An extra stat is never hurtful; all servers
	// are a bundle of race conditions and there's no need
	// to make things worse.
	if len(names) == 0 {
		c := &cpu9p{path: last.path}
		qid, fi, err := c.info()
		verbose("Walk to %v: %v, %v, %v", *c, qid, fi, err)
		if err != nil {
			return nil, nil, err
		}
		qids = append(qids, qid)
		verbose("Walk: return %v, %v, nil", qids, last)
		return qids, last, nil
	}
	verbose("Walk: %v", names)
	for _, name := range names {
		c := &cpu9p{path: filepath.Join(last.path, name)}
		qid, fi, err := c.info()
		verbose("Walk to %v: %v, %v, %v", *c, qid, fi, err)
		if err != nil {
			return nil, nil, err
		}
		qids = append(qids, qid)
		last = c
	}
	verbose("Walk: return %v, %v, nil", qids, last)
	return qids, last, nil
}

// FSync implements p9.File.FSync.
func (l *cpu9p) FSync() error {
	return l.file.Sync()
}

// Close implements p9.File.Close.
func (l *cpu9p) Close() error {
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// Open implements p9.File.Open.
func (l *cpu9p) Open(mode p9.OpenFlags) (p9.QID, uint32, error) {
	qid, fi, err := l.info()
	verbose("Open %v: (%v, %v, %v", *l, qid, fi, err)
	if err != nil {
		return qid, 0, err
	}

	flags := osflags(fi, mode)
	// Do the actual open.
	f, err := os.OpenFile(l.path, flags, 0)
	verbose("Open(%v, %v, %v): (%v, %v", l.path, flags, 0, f, err)
	if err != nil {
		return qid, 0, err
	}
	l.file = f
	// from DIOD
	// if iounit=0, v9fs will use msize-P9_IOHDRSZ
	verbose("Open returns %v, 0, nil", qid)
	return qid, 0, nil
}

// Read implements p9.File.ReadAt.
func (l *cpu9p) ReadAt(p []byte, offset int64) (int, error) {
	switch l.pendingXattr.op {
	case xattrNone:
		return l.file.ReadAt(p, int64(offset))
	case xattrWalk:
		if len(p) == 0 {
			return 0, nil
		}
		if offset != 0 {
			return 0, syscall.EINVAL
		}
		if l.pendingXattr.name == "" {
			return unix.Llistxattr(l.path, p)
		}
		return unix.Lgetxattr(l.path, l.pendingXattr.name, p)
	default:
		return 0, syscall.EINVAL
	}
}

// Write implements p9.File.WriteAt.
// There is a very rare case where O_APPEND files are written more than
// once, and we get an error. That error is generated by the Go runtime,
// after checking the open flag in the os.File struct.
// I.e. the error is not generated by a system call,
// so it is very cheap to try the WriteAt, check the
// error, and call Write if it is the rare case of a second write
// to an append-only file..
func (l *cpu9p) WriteAt(p []byte, offset int64) (int, error) {
	switch l.pendingXattr.op {
	case xattrNone:
		n, err := l.file.WriteAt(p, int64(offset))
		if err != nil {
			if strings.Contains(err.Error(), "os: invalid use of WriteAt on file opened with O_APPEND") {
				return l.file.Write(p)
			}
		}
		return n, err
	case xattrCreate:
		if offset != 0 {
			return 0, syscall.EINVAL
		}
		flags := int(l.pendingXattr.flags)
		return int(l.pendingXattr.size), unix.Lsetxattr(l.path, l.pendingXattr.name, p, flags)
	default:
		return 0, syscall.EINVAL
	}

}

// Create implements p9.File.Create.
func (l *cpu9p) Create(name string, mode p9.OpenFlags, permissions p9.FileMode, _ p9.UID, _ p9.GID) (p9.File, p9.QID, uint32, error) {
	f, err := os.OpenFile(filepath.Join(l.path, name), os.O_CREATE|mode.OSFlags(), os.FileMode(permissions))
	if err != nil {
		return nil, p9.QID{}, 0, err
	}

	l2 := &cpu9p{path: filepath.Join(l.path, name), file: f}
	qid, _, err := l2.info()
	if err != nil {
		l2.Close()
		return nil, p9.QID{}, 0, err
	}

	// from DIOD
	// if iounit=0, v9fs will use msize-P9_IOHDRSZ
	return l2, qid, 0, nil
}

// Mkdir implements p9.File.Mkdir.
//
// Not properly implemented.
func (l *cpu9p) Mkdir(name string, permissions p9.FileMode, _ p9.UID, _ p9.GID) (p9.QID, error) {
	if err := os.Mkdir(filepath.Join(l.path, name), os.FileMode(permissions)); err != nil {
		return p9.QID{}, err
	}

	// Blank QID.
	return p9.QID{}, nil
}

// Symlink implements p9.File.Symlink.
//
// Not properly implemented.
func (l *cpu9p) Symlink(oldname string, newname string, _ p9.UID, _ p9.GID) (p9.QID, error) {
	if err := os.Symlink(oldname, filepath.Join(l.path, newname)); err != nil {
		return p9.QID{}, err
	}

	// Blank QID.
	return p9.QID{}, nil
}

// Link implements p9.File.Link.
//
// Not properly implemented.
func (l *cpu9p) Link(target p9.File, newname string) error {
	return os.Link(target.(*cpu9p).path, filepath.Join(l.path, newname))
}

// Readdir implements p9.File.Readdir.
func (l *cpu9p) Readdir(offset uint64, count uint32) (p9.Dirents, error) {
	fi, err := ioutil.ReadDir(l.path)
	if err != nil {
		return nil, err
	}
	var dirents p9.Dirents
	//log.Printf("readdir %q returns %d entries start at offset %d", l.path, len(fi), offset)
	for i := int(offset); i < len(fi); i++ {
		entry := cpu9p{path: filepath.Join(l.path, fi[i].Name())}
		qid, _, err := entry.info()
		if err != nil {
			continue
		}
		dirents = append(dirents, p9.Dirent{
			QID:    qid,
			Type:   qid.Type,
			Name:   fi[i].Name(),
			Offset: uint64(i + 1),
		})
	}

	return dirents, nil
}

// Readlink implements p9.File.Readlink.
func (l *cpu9p) Readlink() (string, error) {
	n, err := os.Readlink(l.path)
	if false && err != nil {
		log.Printf("Readlink(%v): %v, %v", *l, n, err)
	}
	return n, err
}

// Flush implements p9.File.Flush.
func (l *cpu9p) Flush() error {
	return nil
}

// Renamed implements p9.File.Renamed.
func (l *cpu9p) Renamed(parent p9.File, newName string) {
	l.path = filepath.Join(parent.(*cpu9p).path, newName)
}

// Remove implements p9.File.Remove
func (l *cpu9p) Remove() error {
	err := os.Remove(l.path)
	verbose("Remove(%q): (%v)", l.path, err)
	return err
}

// UnlinkAt implements p9.File.UnlinkAt.
// The flags docs are not very clear, but we
// always block on the unlink anyway.
func (l *cpu9p) UnlinkAt(name string, flags uint32) error {
	f := filepath.Join(l.path, name)
	err := os.Remove(f)
	verbose("UnlinkAt(%q=(%q, %q), %#x): (%v)", f, l.path, name, flags, err)
	return err
}

// Mknod implements p9.File.Mknod.
func (*cpu9p) Mknod(name string, mode p9.FileMode, major uint32, minor uint32, _ p9.UID, _ p9.GID) (p9.QID, error) {
	verbose("Mknod: not implemented")
	return p9.QID{}, syscall.ENOSYS
}

// Rename implements p9.File.Rename.
func (*cpu9p) Rename(directory p9.File, name string) error {
	verbose("Rename: not implemented")
	return syscall.ENOSYS
}

// RenameAt implements p9.File.RenameAt.
// There is no guarantee that there is not a zipslip issue.
func (l *cpu9p) RenameAt(oldName string, newDir p9.File, newName string) error {
	oldPath := path.Join(l.path, oldName)
	nd, ok := newDir.(*cpu9p)
	if !ok {
		// This is extremely serious and points to an internal error.
		// Hence the non-optional log.Printf. It should not ever happen.
		log.Printf("Can not happen: cast of newDir to %T failed; it is type %T", l, newDir)
		return os.ErrInvalid
	}
	newPath := path.Join(nd.path, newName)

	return os.Rename(oldPath, newPath)
}

// StatFS implements p9.File.StatFS.
//
// Not implemented.
func (*cpu9p) StatFS() (p9.FSStat, error) {
	verbose("StatFS: not implemented")
	return p9.FSStat{}, syscall.ENOSYS
}
