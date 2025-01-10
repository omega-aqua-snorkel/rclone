// Package files implements io/fs objects
package files

import (
	"context"
	"fmt"
	"io"
	stdfs "io/fs"
	"time"

	"archive/tar"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/operations"
)

// not using this, not all backends have uid/gid so is this even worth it?

/*

func metadataToHeader(metadata map[string]string,header *tar.Header){
	var val string
	var ok bool
	var err error
	var mode,uid,gid int64
	var atime,ctime time.Time
	// check if metadata is valid
	mode=0644
	uid=0
	gid=0
	atime=time.Unix(0,0)
	ctime=time.Unix(0,0)
	if metadata != nil {
		// mode
	        val, ok = metadata["mode"]
	        if ok == false {
			mode=0644
		}else{
		        mode, err = strconv.ParseInt(val, 8, 64)
		        if err != nil { mode = 0664 }
		}
		// uid
	        val, ok = metadata["uid"]
	        if ok == false {
			uid=0
		}else{
		        uid, err = strconv.ParseInt(val, 10, 32)
		        if err != nil { uid = 0 }
		}
		// gid
	        val, ok = metadata["gid"]
	        if ok == false {
			gid=0
		}else{
		        gid, err = strconv.ParseInt(val, 10, 32)
		        if err != nil { gid = 0 }
		}
		// access time
	        val, ok := metadata["atime"]
	        if ok == false {
			atime=time.Unix(0,0)
		}else{
		        atime, err = time.Parse(time.RFC3339Nano,val)
		        if err != nil { atime = time.Unix(0,0) }
		}
	}
	//
	header.Mode = mode
	header.Uid = int(uid)
	header.Gid = int(gid)
	header.AccessTime = atime
	header.ChangeTime = ctime
}

*/

// structs for fs.FileInfo,fs.File,SeekableFile

type fileInfoImpl struct {
	name   string
	size   int64
	mtime  time.Time
	isDir  bool
	header *tar.Header // hate this, just for uid/gid/gname/uname
}

type fileImpl struct {
	entry    stdfs.FileInfo
	ctx      context.Context
	reader   io.ReadCloser
	transfer *accounting.Transfer
	err      error
}

type seekableFileImpl struct {
	src *operations.ReOpen
}

// NewFileInfo - create a fs.FileInfo compatible struct
func NewFileInfo(name string, size int64, mtime time.Time, isDir bool) stdfs.FileInfo {
	var fi = new(fileInfoImpl)
	//
	fi.name = name
	fi.size = size
	fi.mtime = mtime
	fi.isDir = isDir
	fi.header = new(tar.Header)
	fi.header.Mode = 0666
	fi.header.Uid = 0
	fi.header.Gid = 0
	fi.header.Uname = "root"
	fi.header.Gname = "root"
	fi.header.AccessTime = mtime
	fi.header.ChangeTime = mtime
	//
	return fi
}


func (a *fileInfoImpl) Name() string {
	return a.name
}

func (a *fileInfoImpl) Size() int64 {
	return a.size
}

func (a *fileInfoImpl) Mode() stdfs.FileMode {
	return stdfs.FileMode(a.header.Mode)
}

func (a *fileInfoImpl) ModTime() time.Time {
	return a.mtime
}

func (a *fileInfoImpl) IsDir() bool {
	return a.isDir
}

func (a *fileInfoImpl) Sys() any {
	return a.header
}

func (a *fileInfoImpl) String() string {
	return fmt.Sprintf("Name=%v Size=%v IsDir=%v UID=%v GID=%v", a.Name(), a.Size(), a.IsDir(), a.header.Uid, a.header.Gid)
}


// NewFile - create a fs.File compatible struct
func NewFile(ctx context.Context, entry stdfs.FileInfo, reader io.ReadCloser, transfer *accounting.Transfer) stdfs.File {
	var f = new(fileImpl)
	//
	f.entry = entry
	f.ctx = ctx
	f.reader = reader
	f.transfer = transfer
	f.err = nil
	//
	return f
}

func (a *fileImpl) Stat() (stdfs.FileInfo, error) {
	return a.entry, nil
}

func (a *fileImpl) Read(data []byte) (int, error) {
	if a.reader == nil {
		a.err = fmt.Errorf("file %s not open", a.entry.Name())
		return 0, a.err
	}
	i, err := a.reader.Read(data)
	a.err = err
	return i, a.err
}

func (a *fileImpl) Close() error {
	// close file
	if a.reader == nil {
		a.err = fmt.Errorf("file %s not open", a.entry.Name())
	} else {
		a.err = a.reader.Close()
	}
	// close transfer
	if a.transfer != nil {
		a.transfer.Done(a.ctx, a.err)
	}
	return a.err
}



// SeekableFile - wrap fs.ReOpen files
type SeekableFile interface {
	io.Reader
	io.Seeker
	io.Closer
	io.ReaderAt
}

// NewSeekableFile - wraps ReOpen file with io.Seeker,io.ReadAt so extract/list works with 7z/zip files
func NewSeekableFile(ctx context.Context, src fs.Object, maxTries int) (SeekableFile, error) {
	var f = new(seekableFileImpl)
	//
	var options []fs.OpenOption
	for _, option := range fs.GetConfig(ctx).DownloadHeaders {
		options = append(options, option)
	}
	// try and open file
	rc, err := operations.NewReOpen(ctx, src, maxTries, options...)
	//
	if err != nil {
		return nil, err
	}
	//
	f.src = rc
	return f, nil
}

func (a *seekableFileImpl) Read(p []byte) (n int, err error) {
	return a.src.Read(p)
}

func (a *seekableFileImpl) Seek(offset int64, whence int) (int64, error) {
	return a.src.Seek(offset, whence)
}

func (a *seekableFileImpl) Close() error {
	return a.src.Close()
}

func (a *seekableFileImpl) ReadAt(p []byte, off int64) (int, error) {
	var err error
	//
	_, err = a.src.Seek(off, io.SeekStart)
	if err != nil {
		return 0, err
	}
	return a.src.Read(p)
}


