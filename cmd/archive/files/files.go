// Package files implements io/fs objects
package files

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	stdfs "io/fs"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mholt/archives"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/operations"
)

const (
	// Opening flag used to inform ProgressHandler that we are opening the file
	Opening = 1
	// Reading flag used to inform ProgressHandler that we are reading from the file
	Reading = 2
	// Closing flag used to inform ProgressHandler that we are closing the file
	Closing = 3
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

// ProgressCallback used to inform the status of the file
type ProgressCallback func(info accounting.TransferSnapshot, action int)

type fileInfoImpl struct {
	header *tar.Header
}

type fileImpl struct {
	entry    stdfs.FileInfo
	ctx      context.Context
	reader   io.ReadCloser
	transfer *accounting.Transfer
	progress ProgressCallback
	err      error
}

func newFileInfo(ctx context.Context, entry fs.DirEntry, prefix string) stdfs.FileInfo {
	var fi = new(fileInfoImpl)
	//
	fi.header = new(tar.Header)
	if prefix != "" {
		fi.header.Name = path.Join(strings.TrimPrefix(prefix, "/"), entry.Remote())
	} else {
		fi.header.Name = entry.Remote()
	}
	fi.header.Size = entry.Size()
	fi.header.Mode = 0666
	_, isDir := entry.(fs.Directory)
	if isDir {
		fi.header.Mode = int64(stdfs.ModeDir) | fi.header.Mode
	}
	fi.header.Uid = 0
	fi.header.Gid = 0
	fi.header.Uname = "root"
	fi.header.Gname = "root"
	fi.header.ModTime = entry.ModTime(ctx)
	fi.header.AccessTime = entry.ModTime(ctx)
	fi.header.ChangeTime = entry.ModTime(ctx)
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

// NewArchiveFileInfo will take a fs.DirEntry and return a archives.Fileinfo
func NewArchiveFileInfo(ctx context.Context, entry fs.DirEntry, prefix string, progress ProgressCallback) archives.FileInfo {
	fi := newFileInfo(ctx, entry, prefix)
	//
	return archives.FileInfo{
		FileInfo:      fi,
		NameInArchive: fi.Name(),
		LinkTarget:    "",
		Open: func() (stdfs.File, error) {
			obj, isObject := entry.(fs.Object)
			if isObject {
				return NewFile(ctx, obj, fi, progress)
			}
			return nil, fmt.Errorf("%s is not a file", fi.Name())
		},
	}

}

// NewFile - create a fs.File compatible struct
func NewFile(ctx context.Context, obj fs.Object, fi stdfs.FileInfo, progress ProgressCallback) (stdfs.File, error) {
	var f = new(fileImpl)
	// create stdfs.File
	f.entry = fi
	f.ctx = ctx
	f.err = nil
	f.progress = progress
	// create transfer
	f.transfer = accounting.Stats(ctx).NewTransfer(obj, nil)
	// get open options
	var options []fs.OpenOption
	for _, option := range fs.GetConfig(ctx).DownloadHeaders {
		options = append(options, option)
	}
	// open file
	f.reader, f.err = operations.Open(ctx, obj, options...)
	if f.err != nil {
		defer f.transfer.Done(ctx, f.err)
		return nil, f.err
	}
	// Account the transfer
	f.reader = f.transfer.Account(ctx, f.reader)
	// refresh
	f.Update(Opening)
	//
	return f, f.err
}

func (a *fileImpl) Update(action int) {
	if a.progress != nil && a.transfer != nil {
		a.progress(a.transfer.Snapshot(), action)
	}
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
	a.Update(Reading)
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
	a.Update(Closing)
	a.transfer.Done(a.ctx, a.err)
	//
	return a.err
}

// CountWriter will counts bytes written
type CountWriter struct {
	count uint64
	io.Writer
}

// NewCountWriter will create a writer that counts bytes written
func NewCountWriter(w io.Writer) *CountWriter {
	return &CountWriter{
		Writer: w,
	}
}

func (w *CountWriter) Write(buf []byte) (int, error) {
	var n int
	var err error
	// if writer is null just count
	if w.Writer == nil {
		n = len(buf)
		err = nil
	} else {
		n, err = w.Writer.Write(buf)
	}
	// add bytes written
	if n >= 0 {
		atomic.AddUint64(&w.count, uint64(n))
	}
	return n, err
}

// Count returns total bytes written to writer
func (w *CountWriter) Count() uint64 {
	return atomic.LoadUint64(&w.count)
}
