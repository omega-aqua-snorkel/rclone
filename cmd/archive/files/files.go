// Package files implements io/fs objects
package files

import (
	"context"
	"fmt"
	"io"
	stdfs "io/fs"
	"time"

	"archive/tar"

	"github.com/rclone/rclone/fs/accounting"
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
	header *tar.Header
}

type fileImpl struct {
	entry    stdfs.FileInfo
	ctx      context.Context
	reader   io.ReadCloser
	transfer *accounting.Transfer
	err      error
}

// NewFileInfo - create a fs.FileInfo compatible struct
func NewFileInfo(name string, size int64, mtime time.Time, isDir bool) stdfs.FileInfo {
	var fi = new(fileInfoImpl)
	//
	fi.header = new(tar.Header)
	fi.header.Name = name
	fi.header.Size = size
	fi.header.Mode = 0666
	if isDir {
		fi.header.Mode = int64(stdfs.ModeDir) | fi.header.Mode
	}
	fi.header.Uid = 0
	fi.header.Gid = 0
	fi.header.Uname = "root"
	fi.header.Gname = "root"
	fi.header.ModTime = mtime
	fi.header.AccessTime = mtime
	fi.header.ChangeTime = mtime
	//
	return fi
}

func (a *fileInfoImpl) Name() string {
	return a.header.Name
}

func (a *fileInfoImpl) Size() int64 {
	return a.header.Size
}

func (a *fileInfoImpl) Mode() stdfs.FileMode {
	return stdfs.FileMode(a.header.Mode)
}

func (a *fileInfoImpl) ModTime() time.Time {
	return a.header.ModTime
}

func (a *fileInfoImpl) IsDir() bool {
	return (a.header.Mode & int64(stdfs.ModeDir)) != 0
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
