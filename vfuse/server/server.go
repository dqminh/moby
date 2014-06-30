// Package server initialize a FUSE filesystem and relays all its
// operations to the client over a TCP connection.

package server

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/dotcloud/docker/vfuse/pb"

	"code.google.com/p/goprotobuf/proto"
	"github.com/dotcloud/docker/vfuse"
	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"github.com/hanwen/go-fuse/fuse/pathfs"
)

var Verbose bool

func vlogf(format string, args ...interface{}) {
	if !Verbose {
		return
	}
	log.Printf("server: "+format, args...)
}

func fuseError(err *pb.Error) fuse.Status {
	if err == nil {
		return fuse.OK
	}
	if err.GetNotExist() {
		return fuse.ENOENT
	}
	if err.GetReadOnly() {
		return fuse.EROFS
	}
	if err.GetNotDir() {
		return fuse.ENOTDIR
	}
	// TODO: more
	return fuse.EIO
}

func pbTime(t *time.Time) *pb.Time {
	sec := t.Unix()
	nsec := int32(t.Nanosecond())
	return &pb.Time{Sec: &sec, Nsec: &nsec}
}

// Server is the FUSE filesystem that relays all operations back to
// the client.
type Server struct {
	*fuse.Server
	Connector *nodefs.FileSystemConnector
}

// NewServer runs a relaying FUSE filesystem at mount.
// The provided clientConn function is called at most once
// and should return a connection connected to the client.
// In practice (in Dockerd) this net.Conn will be hijacked
// from an HTTP request when the client goes to attach
// to a filesystem.
func NewServer(mount string, clientConn func() net.Conn) (*Server, error) {
	opts := &fuse.MountOptions{
		Name: "vfuse_SOMECLIENT",
	}
	_ = opts

	fs := newFS(clientConn)
	nfs := pathfs.NewPathNodeFs(fs, nil)

	log.Printf("Mounting at %s", mount)
	srv, fsConnector, err := nodefs.MountRoot(mount, nfs.Root(), nil)
	if err != nil {
		return nil, fmt.Errorf("NewServer: %v", err)
	}
	return &Server{
		Server:    srv,
		Connector: fsConnector,
	}, nil
}

// FS is the implementation of the the pathfs.FileSystem interface.
// It relays all its operations back to the client.
// See http://godoc.org/github.com/hanwen/go-fuse/fuse/pathfs#FileSystem
type FS struct {
	pathfs.FileSystem

	clientOnce sync.Once       // guards calling (*FS).initClient
	clientConn func() net.Conn // called once in initClient
	vc         *vfuse.Client

	mu     sync.Mutex // guards the following fields
	nextid uint64
	res    map[uint64]chan<- proto.Message
}

func newFS(clientConn func() net.Conn) *FS {
	return &FS{
		FileSystem: pathfs.NewDefaultFileSystem(),
		clientConn: clientConn,
		res:        make(map[uint64]chan<- proto.Message),
	}
}

func (fs *FS) initClient() {
	vlogf("server: initClient")
	c := fs.clientConn()
	vlogf("server: init got client %v from %v", c, c.RemoteAddr())
	fs.vc = vfuse.NewClient(c)
	go fs.readFromClient()
}

func (fs *FS) sendPacket(body proto.Message) (<-chan proto.Message, error) {
	fs.clientOnce.Do(fs.initClient)
	id, resc := fs.nextID()
	if err := fs.vc.WritePacket(vfuse.Packet{
		Header: vfuse.Header{
			ID: id,
		},
		Body: body,
	}); err != nil {
		return nil, err
	}
	return resc, nil
}

func (fs *FS) nextID() (uint64, <-chan proto.Message) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	c := make(chan proto.Message, 1)
	id := fs.nextid
	fs.nextid++
	fs.res[id] = c
	return id, c
}

func (fs *FS) readFromClient() {
	for {
		p, err := fs.vc.ReadPacket()
		if err != nil {
			log.Printf("fuse server: error reading client packet: %v", err)
			return
		}
		id := p.Header.ID
		fs.mu.Lock()
		resc, ok := fs.res[id]
		if ok {
			fs.forgetRequestLocked(id)
		}
		fs.mu.Unlock()
		if !ok {
			log.Printf("fuse server: client sent bogus packet we didn't ask for; aborting")
			return
		}
		resc <- p.Body
	}
}

func (fs *FS) forgetRequestLocked(id uint64) {
	delete(fs.res, id)
}

func (fs *FS) Chmod(name string, mode uint32, context *fuse.Context) fuse.Status {
	vlogf("fs.Chmod(%q)", name)
	resc, err := fs.sendPacket(&pb.ChmodRequest{
		Name: &name,
		Mode: &mode,
	})
	if err != nil {
		return fuse.EIO
	}
	res, ok := (<-resc).(*pb.ChmodResponse)
	if !ok {
		vlogf("fs.Chmod(%q) = EIO because wrong type", name)
		return fuse.EIO
	}
	return fuseError(res.Err)
}

func (fs *FS) GetAttr(name string, context *fuse.Context) (*fuse.Attr, fuse.Status) {
	vlogf("fs.GetAttr(%q)", name)

	// Docker stats this before it returns to the client the filesystem ID.
	// So fake stat of the root:
	if name == "" {
		const S_IFDIR = 0x4000
		return &fuse.Attr{
			Mode:    0755 | S_IFDIR,
			Nlink:   1,
			Blksize: 1024,
			Blocks:  1,
		}, fuse.OK
	}

	resc, err := fs.sendPacket(&pb.AttrRequest{
		Name: &name,
	})
	if err != nil {
		return nil, fuse.EIO
	}
	resi := <-resc
	vlogf("fs.GetAttr(%q) read response %T, %v", name, resi, resi)
	res, ok := resi.(*pb.AttrResponse)
	if !ok {
		vlogf("fs.GetAttr(%q) = EIO because wrong type", name)
		return nil, fuse.EIO
	}
	if res.Err != nil {
		return nil, fuseError(res.Err)
	}
	attr := res.Attr
	if attr == nil {
		vlogf("fs.GetAttr(%q) = EIO because nil Attr", name)
		return nil, fuse.EIO
	}
	fattr := &fuse.Attr{
		Size:    attr.GetSize(),
		Mode:    attr.GetMode(),
		Nlink:   1,
		Blksize: 1024,
		Blocks:  attr.GetSize() / 1024,
	}
	vlogf("fs.GetAttr(%q) = OK: %+v", name, fattr)
	return fattr, fuse.OK
}

func (fs *FS) Mkdir(name string, mode uint32, context *fuse.Context) fuse.Status {
	vlogf("fs.Mkdir(%q, %o)", name, mode)
	resc, err := fs.sendPacket(&pb.MkdirRequest{
		Name: &name,
		Mode: &mode,
	})
	if err != nil {
		return fuse.EIO
	}
	res, ok := (<-resc).(*pb.MkdirResponse)
	if !ok {
		vlogf("fs.Mkdir(%q) = EIO because wrong type", name)
	}
	return fuseError(res.Err)
}

func (fs *FS) Open(name string, flags uint32, context *fuse.Context) (nodefs.File, fuse.Status) {
	vlogf("fs.Open(%q, flags %d)", name, flags)
	resc, err := fs.sendPacket(&pb.OpenRequest{
		Name:  &name,
		Flags: &flags,
	})
	if err != nil {
		return nil, fuse.EIO
	}
	res, ok := (<-resc).(*pb.OpenResponse)
	if !ok {
		return nil, fuse.EIO
	}
	vlogf("Open(%q, flags %d) = %v", name, flags, res)
	if res.Err != nil {
		return nil, fuseError(res.Err)
	}
	f := &file{
		fs:        fs,
		File:      nodefs.NewDefaultFile(), // dummy ops for everything
		handle:    res.GetHandle(),
		origName:  name,
		origFlags: flags,
	}
	if f.handle == 0 {
		return nil, fuse.EIO
	}
	return f, fuse.OK
}

func (fs *FS) OpenDir(name string, context *fuse.Context) (stream []fuse.DirEntry, code fuse.Status) {
	vlogf("fs.OpenDir(%q) ...", name)
	resc, err := fs.sendPacket(&pb.ReaddirRequest{
		Name: &name,
	})
	if err != nil {
		vlogf("OpenDir error = %v", err)
		return nil, fuse.EIO
	}
	resi := <-resc
	res, ok := resi.(*pb.ReaddirResponse)
	if !ok {
		vlogf("OpenDir type error; was %T", resi)
		return nil, fuse.EIO
	}
	vlogf("fs.OpenDir(%q) = %v", name, res)
	stream = make([]fuse.DirEntry, len(res.Entry))
	for i, ent := range res.Entry {
		stream[i] = fuse.DirEntry{
			Name: ent.GetName(),
			Mode: ent.GetMode(),
		}
	}
	return stream, fuseError(res.Err)
}

func (fs *FS) Readlink(name string, context *fuse.Context) (string, fuse.Status) {
	vlogf("fs.Readlink(%q)", name)
	resc, err := fs.sendPacket(&pb.ReadlinkRequest{
		Name: &name,
	})
	if err != nil {
		return "", fuse.EIO
	}
	res, ok := (<-resc).(*pb.ReadlinkResponse)
	if !ok {
		vlogf("fs.Readlink(%q) = EIO because wrong type", name)
		return "", fuse.EIO
	}
	if res.Err != nil {
		return "", fuseError(res.Err)
	}
	return res.GetTarget(), fuse.OK
}

func (fs *FS) Rename(name string, target string, context *fuse.Context) fuse.Status {
	vlogf("fs.Rename(%q, %q)", name, target)
	resc, err := fs.sendPacket(&pb.RenameRequest{
		Name:   &name,
		Target: &target,
	})
	if err != nil {
		return fuse.EIO
	}
	res, ok := (<-resc).(*pb.RenameResponse)
	if !ok {
		vlogf("fs.Rename(%q, %q) = EIO", name, target)
		return fuse.EIO
	}
	return fuseError(res.Err)
}

func (fs *FS) Rmdir(name string, context *fuse.Context) fuse.Status {
	vlogf("fs.Rmdir(%q)", name)
	resc, err := fs.sendPacket(&pb.RmdirRequest{
		Name: &name,
	})
	if err != nil {
		return fuse.EIO
	}
	res, ok := (<-resc).(*pb.RmdirResponse)
	if !ok {
		vlogf("fs.Rmdir(%q) = EIO because wrong type", name)
	}
	return fuseError(res.Err)
}

func (fs *FS) Symlink(value string, linkName string, context *fuse.Context) fuse.Status {
	vlogf("fs.Symlink(%q, %q)", value, linkName)
	resc, err := fs.sendPacket(&pb.SymlinkRequest{
		Value: &value,
		Name:  &linkName,
	})
	if err != nil {
		return fuse.EIO
	}
	res, ok := (<-resc).(*pb.SymlinkResponse)
	if !ok {
		vlogf("fs.Symlink(%q, %q) = EIO", value, linkName)
		return fuse.EIO
	}
	return fuseError(res.Err)
}

func (fs *FS) Utimens(name string, atime *time.Time, mtime *time.Time, context *fuse.Context) fuse.Status {
	vlogf("fs.Utimens(%q, atime: %v, mtime: %v)", name, atime, mtime)
	resc, err := fs.sendPacket(&pb.UtimeRequest{
		Name:  &name,
		Atime: pbTime(atime),
		Mtime: pbTime(mtime),
	})
	if err != nil {
		return fuse.EIO
	}
	res, ok := (<-resc).(*pb.UtimeResponse)
	if !ok {
		vlogf("fs.Utimens(%q, %v, %v) = EIO because wrong type", name, atime, mtime)
		return fuse.EIO
	}
	return fuseError(res.Err)
}

func (fs *FS) Mknod(name string, mode uint32, dev uint32, context *fuse.Context) fuse.Status {
	vlogf("fs.Mknod(%q, mode: %d, dev: %d)", name, mode, dev)
	resc, err := fs.sendPacket(&pb.MknodRequest{
		Name: &name,
		Mode: &mode,
		Dev:  &dev,
	})
	if err != nil {
		return fuse.EIO
	}
	res, ok := (<-resc).(*pb.MknodResponse)
	if !ok {
		vlogf("fs.Mknod(%q, %d, %d) = EIO because wrong type", name, mode, dev)
		return fuse.EIO
	}
	return fuseError(res.Err)
}

func (fs *FS) Unlink(name string, context *fuse.Context) fuse.Status {
	vlogf("fs.Unlink(%q)", name)
	resc, err := fs.sendPacket(&pb.UnlinkRequest{
		Name: &name,
	})
	if err != nil {
		return fuse.EIO
	}
	res, ok := (<-resc).(*pb.UnlinkResponse)
	if !ok {
		vlogf("fs.Unlink(%q) = EIO because wrong type", name)
		return fuse.EIO
	}

	return fuseError(res.Err)
}

func (fs *FS) Truncate(name string, size uint64, context *fuse.Context) fuse.Status {
	vlogf("fs.Truncate(%q, %d)", name, size)
	resc, err := fs.sendPacket(&pb.TruncateRequest{
		Name: &name,
		Size: &size,
	})
	if err != nil {
		return fuse.EIO
	}
	res, ok := (<-resc).(*pb.TruncateResponse)
	if !ok {
		vlogf("fs.Truncate(%q, %d) = EIO because wrong type", name, size)
		return fuse.EIO
	}

	return fuseError(res.Err)
}

func (fs *FS) StatFs(name string) *fuse.StatfsOut {
	vlogf("fs.StatFs(%q)", name)
	out := new(fuse.StatfsOut)
	// TODO(bradfitz): make up some stuff for now. Do this properly later
	// with a new packet type to the client.
	out.Bsize = 1024
	out.Blocks = 1e6
	out.Bfree = out.Blocks / 2
	out.Bavail = out.Blocks / 2
	out.Files = 1e3
	out.Ffree = 1e3 - 2
	return out
}

// file implements http://godoc.org/github.com/hanwen/go-fuse/fuse/nodefs#File
//
// It represents an open file on the filesystem host, identified by
// the filesystem host-assigned handle.
//
// Actually *file implements nodefs.File. The file struct isn't mutated, though.
type file struct {
	nodefs.File
	fs        *FS
	handle    uint64
	origName  string // just for debugging
	origFlags uint32 // just for debugging
}

func (f *file) Flush() fuse.Status {
	resc, err := f.fs.sendPacket(&pb.CloseRequest{
		Handle: &f.handle,
	})
	if err != nil {
		return fuse.EIO
	}
	res, ok := (<-resc).(*pb.CloseResponse)
	if !ok {
		vlogf("fs.Close = EIO due to wrong type")
		return fuse.EIO
	}
	return fuseError(res.Err)
}

func (f *file) Read(dest []byte, off int64) (fuse.ReadResult, fuse.Status) {
	vlogf("fs.Read(offset=%d, size=%d)", off, len(dest))
	resc, err := f.fs.sendPacket(&pb.ReadRequest{
		Handle: &f.handle,
		Offset: proto.Uint64(uint64(off)),
		Size:   proto.Uint64(uint64(len(dest))),
	})
	if err != nil {
		return nil, fuse.EIO
	}
	res, ok := (<-resc).(*pb.ReadResponse)
	if !ok {
		vlogf("fs.Read = EIO due to wrong type")
		return nil, fuse.EIO
	}
	return fuse.ReadResultData(res.Data), fuse.OK
}

func (f *file) Truncate(size uint64) fuse.Status {
	vlogf("fs.Truncate(size=%d)", size)
	resc, err := f.fs.sendPacket(&pb.TruncateRequest{
		Handle: &f.handle,
		Size:   &size,
	})
	if err != nil {
		return fuse.EIO
	}
	res, ok := (<-resc).(*pb.TruncateResponse)
	if !ok {
		vlogf("fs.Truncate(size=%d) = EIO due to wrong type", size)
		return fuse.EIO
	}
	return fuseError(res.Err)
}
